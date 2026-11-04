package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const tempPrefix = ".ccsync-tmp-"

type outcome int

const (
	inSync   outcome = iota
	copied           // at least one account was missing the file
	merged           // transcripts had diverged and lines were exchanged
	replaced         // an older copy was overwritten by the newest one
)

// copyState describes one account's copy of a file; info is nil if it has none.
type copyState struct {
	path string
	info os.FileInfo
}

// syncFile makes every copy of one file equivalent. With apply false it only
// reports what would happen.
func syncFile(copies []copyState, apply bool) (outcome, error) {
	if strings.HasSuffix(copies[0].path, ".jsonl") {
		return syncTranscript(copies, apply)
	}
	return syncNewestWins(copies, apply)
}

// syncNewestWins handles sidecar files such as tool results and file-history
// backups, which are not append-only.
func syncNewestWins(copies []copyState, apply bool) (outcome, error) {
	newest := newestCopy(copies)
	result := inSync
	for _, c := range copies {
		if c.info == nil {
			result = max(result, copied)
			if apply {
				if err := copyAtomic(newest.path, c.path, newest.info); err != nil {
					return result, err
				}
			}
			continue
		}
		same, err := sameContent(newest, c)
		if err != nil {
			return result, err
		}
		if same {
			continue
		}
		result = max(result, replaced)
		if apply {
			if err := copyAtomic(newest.path, c.path, newest.info); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

// syncTranscript treats a JSONL transcript as an unordered set of lines and
// gives every account the union. Lines are compared as raw bytes, so this does
// not depend on the transcript schema, and existing lines are never rewritten:
// a copy only ever grows.
func syncTranscript(copies []copyState, apply bool) (outcome, error) {
	if transcriptsSettled(copies) {
		return inSync, nil
	}

	type loaded struct {
		copyState
		data  []byte
		lines [][]byte
		set   map[[32]byte]bool
	}

	var present []*loaded
	for _, c := range copies {
		if c.info == nil {
			continue
		}
		data, err := os.ReadFile(c.path)
		if err != nil {
			return inSync, err
		}
		l := &loaded{copyState: c, data: data, lines: shareableLines(data), set: map[[32]byte]bool{}}
		for _, line := range l.lines {
			l.set[sha256.Sum256(line)] = true
		}
		present = append(present, l)
	}

	result := inSync
	latest := newestCopy(copies).info.ModTime()

	for _, target := range present {
		var missing []byte
		for _, source := range present {
			if source == target {
				continue
			}
			for _, line := range source.lines {
				sum := sha256.Sum256(line)
				if target.set[sum] {
					continue
				}
				target.set[sum] = true
				missing = append(missing, line...)
				missing = append(missing, '\n')
			}
		}
		if len(missing) == 0 {
			continue
		}
		result = merged
		if !apply {
			continue
		}
		if len(target.data) > 0 && target.data[len(target.data)-1] != '\n' {
			// Terminate a line left unfinished by a crashed writer so it
			// cannot swallow the first appended record.
			missing = append([]byte{'\n'}, missing...)
		}
		if err := appendBytes(target.path, missing); err != nil {
			return result, err
		}
	}

	source := largestCopy(copies)
	for _, c := range copies {
		if c.info != nil {
			continue
		}
		result = max(result, copied)
		if apply {
			if err := mergedCopy(source, c.path); err != nil {
				return result, err
			}
		}
	}

	if apply {
		// Equal mtimes (with equal sizes) let the next run skip this file
		// without reading it, and keep old sessions from jumping to the top
		// of the resume list.
		for _, c := range copies {
			if err := os.Chtimes(c.path, time.Time{}, latest); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

// mergedCopy runs after appends, so the source is re-read from disk to include
// lines that were just merged into it.
func mergedCopy(source copyState, dst string) error {
	info, err := os.Stat(source.path)
	if err != nil {
		return err
	}
	return copyAtomic(source.path, dst, info)
}

// transcriptsSettled is the fast path: every account has the file, with the
// same size and mtime. Merged copies can differ in line order, so this cannot
// be replaced by a content comparison. Sync stamps equal mtimes itself; copies
// that diverged independently will not match on both size and nanosecond mtime.
func transcriptsSettled(copies []copyState) bool {
	first := copies[0].info
	if first == nil {
		return false
	}
	for _, c := range copies[1:] {
		if c.info == nil || c.info.Size() != first.Size() || !c.info.ModTime().Equal(first.ModTime()) {
			return false
		}
	}
	return true
}

// shareableLines returns the lines fit to give to another account. A trailing
// line with no newline may still be being written, and a line that is not valid
// JSON is damaged, including a partial line that an earlier merge terminated.
func shareableLines(data []byte) [][]byte {
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil
	}
	var lines [][]byte
	for _, line := range bytes.Split(data[:end], []byte{'\n'}) {
		if json.Valid(line) {
			lines = append(lines, line)
		}
	}
	return lines
}

func newestCopy(copies []copyState) copyState {
	var best copyState
	for _, c := range copies {
		if c.info != nil && (best.info == nil || c.info.ModTime().After(best.info.ModTime())) {
			best = c
		}
	}
	return best
}

func largestCopy(copies []copyState) copyState {
	var best copyState
	for _, c := range copies {
		if c.info != nil && (best.info == nil || c.info.Size() > best.info.Size()) {
			best = c
		}
	}
	return best
}

func sameContent(a, b copyState) (bool, error) {
	if a.path == b.path {
		return true, nil
	}
	if a.info.Size() != b.info.Size() {
		return false, nil
	}
	if a.info.ModTime().Equal(b.info.ModTime()) {
		return true, nil
	}
	left, err := os.ReadFile(a.path)
	if err != nil {
		return false, err
	}
	right, err := os.ReadFile(b.path)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func appendBytes(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// copyAtomic writes to a temp file beside dst and renames it into place, so a
// crash cannot leave a truncated session. Source mode and mtime are preserved.
func copyAtomic(src, dst string, info os.FileInfo) (err error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	if _, err = io.Copy(tmp, in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err = tmp.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chtimes(tmp.Name(), time.Time{}, info.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
