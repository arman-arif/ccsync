package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// activeWindow catches sessions writing to their transcript without a registry
// entry, such as a process from another PID namespace.
const activeWindow = 30 * time.Second

// Options selects what a run covers.
type Options struct {
	Accounts []Account       // accounts to keep in step
	Projects []string        // encoded project folder names; empty means all
	Live     map[string]bool // session IDs never touched in this run
	Apply    bool            // false reports without writing
	Now      time.Time
}

// SessionResult is the outcome for one session across all accounts.
type SessionResult struct {
	Project string
	ID      string
	Title   string
	Status  Status
	From    []string // accounts that had the transcript
	To      []string // accounts that did not
	Err     error
}

type Status string

const (
	StatusInSync  Status = "in-sync"
	StatusCopy    Status = "copy"
	StatusMerge   Status = "merge"
	StatusUpdate  Status = "update" // only sidecar files differed
	StatusSkipped Status = "skipped"
)

// Sync brings every session in scope into step across the accounts.
func Sync(opts Options) ([]SessionResult, error) {
	sessions, err := findSessions(opts)
	if err != nil {
		return nil, err
	}
	results := make([]SessionResult, 0, len(sessions))
	for _, session := range sessions {
		results = append(results, syncSession(session, opts))
	}
	return results, nil
}

type sessionKey struct{ project, id string }

func findSessions(opts Options) ([]sessionKey, error) {
	wanted := map[string]bool{}
	for _, project := range opts.Projects {
		wanted[project] = true
	}

	seen := map[sessionKey]bool{}
	for _, account := range opts.Accounts {
		root := filepath.Join(account.Dir, "projects")
		projects, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, project := range projects {
			if !project.IsDir() || (len(wanted) > 0 && !wanted[project.Name()]) {
				continue
			}
			entries, err := os.ReadDir(filepath.Join(root, project.Name()))
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				id, isTranscript := strings.CutSuffix(entry.Name(), ".jsonl")
				if isTranscript && entry.Type().IsRegular() {
					seen[sessionKey{project.Name(), id}] = true
				}
			}
		}
	}

	sessions := make([]sessionKey, 0, len(seen))
	for key := range seen {
		sessions = append(sessions, key)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].project != sessions[j].project {
			return sessions[i].project < sessions[j].project
		}
		return sessions[i].id < sessions[j].id
	})
	return sessions, nil
}

func syncSession(session sessionKey, opts Options) SessionResult {
	result := SessionResult{Project: session.project, ID: session.id, Status: StatusInSync}
	transcriptRel := filepath.Join("projects", session.project, session.id+".jsonl")

	var newest time.Time
	var transcript string
	for _, account := range opts.Accounts {
		path := filepath.Join(account.Dir, transcriptRel)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			result.To = append(result.To, account.Name)
			continue
		}
		result.From = append(result.From, account.Name)
		transcript = path
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}

	// A negative age is a clock-skewed mtime, not a session being written to.
	age := opts.Now.Sub(newest)
	if opts.Live[session.id] || (age >= 0 && age < activeWindow) {
		result.Status = StatusSkipped
		result.Title = sessionTitle(transcript)
		return result
	}

	rels, err := sessionFiles(session, opts.Accounts)
	if err != nil {
		result.Err = err
		return result
	}

	// The transcript goes last: once it exists in an account, Claude Code
	// can list the session, so everything it refers to should already be there.
	rels = append(rels, transcriptRel)

	var transcriptOutcome, sidecars outcome
	var errs []error
	for _, rel := range rels {
		copies := make([]copyState, len(opts.Accounts))
		for i, account := range opts.Accounts {
			path := filepath.Join(account.Dir, rel)
			copies[i] = copyState{path: path}
			if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
				copies[i].info = info
			}
		}
		got, err := syncFile(copies, opts.Apply)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if rel == transcriptRel {
			transcriptOutcome = got
		} else {
			sidecars = max(sidecars, got)
		}
	}
	result.Err = errors.Join(errs...)

	switch {
	case transcriptOutcome == merged:
		result.Status = StatusMerge
	case transcriptOutcome == copied:
		result.Status = StatusCopy
	case sidecars != inSync:
		result.Status = StatusUpdate
	}
	// Titles need a full read of the transcript, so in-sync sessions, which
	// are never displayed, keep the fast path by not looking one up.
	if result.Status != StatusInSync {
		result.Title = sessionTitle(transcript)
	}
	return result
}

// sessionFiles lists the config-dir-relative paths of a session's sidecar files
// across all accounts: subagents, tool results and the /rewind file history.
// Only regular files are returned, so symlinks are never followed out of the
// session's own folders.
func sessionFiles(session sessionKey, accounts []Account) ([]string, error) {
	roots := []string{
		filepath.Join("projects", session.project, session.id),
		filepath.Join("file-history", session.id),
	}
	seen := map[string]bool{}
	for _, account := range accounts {
		for _, root := range roots {
			err := filepath.WalkDir(filepath.Join(account.Dir, root), func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), tempPrefix) {
					return nil
				}
				rel, err := filepath.Rel(account.Dir, path)
				if err != nil {
					return err
				}
				seen[rel] = true
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	rels := make([]string, 0, len(seen))
	for rel := range seen {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	return rels, nil
}

// sessionTitle returns the latest title recorded in a transcript. A custom
// title beats an AI-generated one. Failure just means the title is left blank.
func sessionTitle(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	var custom, generated string
	reader := bufio.NewReaderSize(file, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"customTitle"`)) || bytes.Contains(line, []byte(`"aiTitle"`)) {
			var record struct {
				Type        string `json:"type"`
				CustomTitle string `json:"customTitle"`
				AITitle     string `json:"aiTitle"`
			}
			if json.Unmarshal(line, &record) == nil {
				switch {
				case record.Type == "custom-title" && record.CustomTitle != "":
					custom = record.CustomTitle
				case record.Type == "ai-title" && record.AITitle != "":
					generated = record.AITitle
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return ""
		}
	}
	if custom != "" {
		return custom
	}
	return generated
}
