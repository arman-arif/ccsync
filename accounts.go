package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Account is one Claude Code config dir (a CLAUDE_CONFIG_DIR value).
type Account struct {
	Name string // "work" for ~/.claude-work, "default" for ~/.claude
	Dir  string
}

// discoverAccounts finds every ~/.claude* directory that holds a projects/
// folder, plus configDir (normally $CLAUDE_CONFIG_DIR) wherever it lives.
func discoverAccounts(home, configDir string) ([]Account, error) {
	candidates, err := filepath.Glob(filepath.Join(home, ".claude*"))
	if err != nil {
		return nil, err
	}
	if configDir != "" {
		candidates = append(candidates, configDir)
	}

	seen := map[string]bool{}
	var accounts []Account
	for _, dir := range candidates {
		info, err := os.Stat(filepath.Join(dir, "projects"))
		if err != nil || !info.IsDir() {
			continue
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		accounts = append(accounts, Account{Name: accountName(resolved), Dir: resolved})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return accounts, nil
}

func accountName(dir string) string {
	base := filepath.Base(dir)
	if base == ".claude" {
		return "default"
	}
	if name, ok := strings.CutPrefix(base, ".claude-"); ok && name != "" {
		return name
	}
	return base
}

// selectAccounts narrows accounts to a comma-separated list of names or paths.
// A path that was not discovered is accepted if it has a projects/ folder.
func selectAccounts(all []Account, spec string) ([]Account, error) {
	if spec == "" {
		return all, nil
	}
	var picked []Account
	seen := map[string]bool{}
	for _, want := range strings.Split(spec, ",") {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		account, err := resolveAccount(all, want)
		if err != nil {
			return nil, err
		}
		if !seen[account.Dir] {
			seen[account.Dir] = true
			picked = append(picked, account)
		}
	}
	return picked, nil
}

func resolveAccount(all []Account, want string) (Account, error) {
	for _, a := range all {
		if a.Name == want {
			return a, nil
		}
	}
	if strings.ContainsRune(want, filepath.Separator) {
		resolved, err := filepath.EvalSymlinks(want)
		if err == nil {
			if info, err := os.Stat(filepath.Join(resolved, "projects")); err == nil && info.IsDir() {
				return Account{Name: accountName(resolved), Dir: resolved}, nil
			}
		}
		return Account{}, fmt.Errorf("%s is not a Claude config dir (no projects/ folder)", want)
	}
	names := make([]string, len(all))
	for i, a := range all {
		names[i] = a.Name
	}
	return Account{}, fmt.Errorf("unknown account %q (found: %s)", want, strings.Join(names, ", "))
}

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]`)

// encodeProject converts a working directory to the folder name Claude Code
// uses for it under projects/.
func encodeProject(path string) string {
	return nonAlphanumeric.ReplaceAllString(path, "-")
}

// liveSessions returns the IDs of sessions whose Claude Code process is still
// running, read from each account's sessions/<pid>.json registry.
func liveSessions(accounts []Account) map[string]bool {
	live := map[string]bool{}
	for _, account := range accounts {
		files, _ := filepath.Glob(filepath.Join(account.Dir, "sessions", "*.json"))
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			var entry struct {
				PID       int    `json:"pid"`
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(data, &entry) != nil || entry.SessionID == "" {
				continue
			}
			if processAlive(entry.PID) {
				live[entry.SessionID] = true
			}
		}
	}
	return live
}

// processAlive errs towards true: a reused PID only delays a session's sync,
// whereas missing a running session could write under an active writer.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
