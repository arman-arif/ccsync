package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	project = "-home-me-app"
	session = "11111111-1111-1111-1111-111111111111"
)

var (
	now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	old = now.Add(-time.Hour)
)

func newAccount(t *testing.T, root, name string) Account {
	t.Helper()
	dir := filepath.Join(root, ".claude-"+name)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Account{Name: name, Dir: dir}
}

func writeFile(t *testing.T, path, content string, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Time{}, modTime); err != nil {
		t.Fatal(err)
	}
}

func transcriptPath(a Account, project, id string) string {
	return filepath.Join(a.Dir, "projects", project, id+".jsonl")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sortedLines(s string) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	sort.Strings(lines)
	return lines
}

// snapshot captures every file under root so a test can prove nothing changed.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = fmt.Sprintf("%s|%d|%s", info.Mode(), info.ModTime().UnixNano(), data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func mustSync(t *testing.T, opts Options) []SessionResult {
	t.Helper()
	if opts.Now.IsZero() {
		opts.Now = now
	}
	results, err := Sync(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("session %s: %v", r.ID, r.Err)
		}
	}
	return results
}

func TestCopiesSessionWithSidecarsToAccountsMissingIt(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", old)
	writeFile(t, filepath.Join(a.Dir, "projects", project, session, "subagents", "agent-1.jsonl"), `{"sub":1}`+"\n", old)
	writeFile(t, filepath.Join(a.Dir, "projects", project, session, "tool-results", "out.txt"), "result", old)
	writeFile(t, filepath.Join(a.Dir, "file-history", session, "abc@v1"), "backup", old)

	results := mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})

	if len(results) != 1 || results[0].Status != StatusCopy {
		t.Fatalf("results = %+v", results)
	}
	for _, rel := range []string{
		filepath.Join("projects", project, session+".jsonl"),
		filepath.Join("projects", project, session, "subagents", "agent-1.jsonl"),
		filepath.Join("projects", project, session, "tool-results", "out.txt"),
		filepath.Join("file-history", session, "abc@v1"),
	} {
		want := readFile(t, filepath.Join(a.Dir, rel))
		if got := readFile(t, filepath.Join(b.Dir, rel)); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}

	info, err := os.Stat(transcriptPath(b, project, session))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("mtime = %v, want source mtime %v", info.ModTime(), old)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestDivergedTranscriptsConvergeWithoutLosingOrRewritingLines(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	base := `{"uuid":"root"}` + "\n"
	fromA := base + `{"uuid":"a1","parentUuid":"root"}` + "\n"
	fromB := base + `{"uuid":"b1","parentUuid":"root"}` + "\n" + `{"type":"last-prompt"}` + "\n"
	writeFile(t, transcriptPath(a, project, session), fromA, old)
	writeFile(t, transcriptPath(b, project, session), fromB, old.Add(time.Minute))

	results := mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})
	if results[0].Status != StatusMerge {
		t.Fatalf("status = %s, want merge", results[0].Status)
	}

	gotA := readFile(t, transcriptPath(a, project, session))
	gotB := readFile(t, transcriptPath(b, project, session))
	if !strings.HasPrefix(gotA, fromA) || !strings.HasPrefix(gotB, fromB) {
		t.Errorf("merge rewrote existing lines:\nA: %q\nB: %q", gotA, gotB)
	}
	if !slices.Equal(sortedLines(gotA), sortedLines(gotB)) {
		t.Errorf("line sets differ:\nA: %q\nB: %q", gotA, gotB)
	}
	if len(sortedLines(gotA)) != 4 {
		t.Errorf("want 4 unique lines, got %q", gotA)
	}
	if strings.Count(gotA, `"uuid":"root"`) != 1 {
		t.Errorf("shared line duplicated: %q", gotA)
	}
}

func TestSecondRunChangesNothing(t *testing.T) {
	root := t.TempDir()
	a, b, c := newAccount(t, root, "a"), newAccount(t, root, "b"), newAccount(t, root, "c")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"root"}`+"\n"+`{"uuid":"a1"}`+"\n", old)
	writeFile(t, transcriptPath(b, project, session), `{"uuid":"root"}`+"\n"+`{"uuid":"b1"}`+"\n", old)
	writeFile(t, filepath.Join(a.Dir, "file-history", session, "abc@v1"), "backup", old)
	accounts := []Account{a, b, c}

	mustSync(t, Options{Accounts: accounts, Apply: true})
	before := snapshot(t, root)

	results := mustSync(t, Options{Accounts: accounts, Apply: true})
	if results[0].Status != StatusInSync {
		t.Errorf("status = %s, want in-sync", results[0].Status)
	}
	if after := snapshot(t, root); !mapsEqual(before, after) {
		t.Errorf("second run changed files")
	}
	if !slices.Equal(sortedLines(readFile(t, transcriptPath(a, project, session))),
		sortedLines(readFile(t, transcriptPath(c, project, session)))) {
		t.Errorf("third account did not receive the merged transcript")
	}
}

func TestStatusDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"a"}`+"\n", old)
	writeFile(t, transcriptPath(b, project, session), `{"uuid":"b"}`+"\n", old.Add(time.Minute))
	writeFile(t, transcriptPath(a, project, "only-a"), `{"uuid":"x"}`+"\n", old)
	writeFile(t, filepath.Join(a.Dir, "file-history", session, "abc@v1"), "backup", old)
	before := snapshot(t, root)

	results := mustSync(t, Options{Accounts: []Account{a, b}, Apply: false})

	if after := snapshot(t, root); !mapsEqual(before, after) {
		t.Errorf("status changed files")
	}
	got := map[string]Status{}
	for _, r := range results {
		got[r.ID] = r.Status
	}
	if got[session] != StatusMerge || got["only-a"] != StatusCopy {
		t.Errorf("statuses = %v", got)
	}
}

func TestRunningSessionsAreLeftAlone(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", old)
	writeFile(t, filepath.Join(a.Dir, "sessions", "1.json"),
		fmt.Sprintf(`{"pid":%d,"sessionId":%q}`, os.Getpid(), session), old)

	live := liveSessions([]Account{a, b})
	results := mustSync(t, Options{Accounts: []Account{a, b}, Live: live, Apply: true})

	if results[0].Status != StatusSkipped {
		t.Errorf("status = %s, want skipped", results[0].Status)
	}
	if _, err := os.Stat(transcriptPath(b, project, session)); !os.IsNotExist(err) {
		t.Errorf("running session was copied")
	}
}

func TestForceSyncsRunningSessions(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", now.Add(-5*time.Second))

	live := map[string]bool{session: true}
	results := mustSync(t, Options{Accounts: []Account{a, b}, Live: live, Force: true, Apply: true})

	if results[0].Status != StatusCopy {
		t.Errorf("status = %s, want copy", results[0].Status)
	}
	if got := readFile(t, transcriptPath(b, project, session)); got != `{"uuid":"1"}`+"\n" {
		t.Errorf("copied transcript = %q", got)
	}
}

func TestDeadProcessDoesNotBlockSync(t *testing.T) {
	root := t.TempDir()
	a := newAccount(t, root, "a")
	writeFile(t, filepath.Join(a.Dir, "sessions", "1.json"),
		fmt.Sprintf(`{"pid":%d,"sessionId":%q}`, 1<<30, session), old)
	if live := liveSessions([]Account{a}); live[session] {
		t.Errorf("session with a dead pid reported as live")
	}
}

func TestRecentlyWrittenSessionIsSkipped(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", now.Add(-5*time.Second))

	results := mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})

	if results[0].Status != StatusSkipped {
		t.Errorf("status = %s, want skipped", results[0].Status)
	}
}

func TestFutureMtimeIsNotTreatedAsRunning(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", now.Add(time.Hour))

	results := mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})

	if results[0].Status != StatusCopy {
		t.Errorf("status = %s, want copy", results[0].Status)
	}
}

func TestProjectScopeLeavesOtherProjectsAlone(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", old)
	writeFile(t, transcriptPath(a, "-home-me-other", "22222222"), `{"uuid":"2"}`+"\n", old)

	mustSync(t, Options{Accounts: []Account{a, b}, Projects: []string{project}, Apply: true})

	if _, err := os.Stat(transcriptPath(b, project, session)); err != nil {
		t.Errorf("project in scope not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "projects", "-home-me-other")); !os.IsNotExist(err) {
		t.Errorf("project out of scope was touched")
	}
}

func TestOnlySessionFilesCross(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", old)
	private := []string{
		".credentials.json", ".claude.json", "settings.json", "history.jsonl",
		filepath.Join("session-env", session, "env.sh"),
		filepath.Join("sessions", "99.json"),
		filepath.Join("projects", project, "memory", "MEMORY.md"),
		filepath.Join("projects", project, "other-session", "stray.txt"),
	}
	for _, rel := range private {
		writeFile(t, filepath.Join(a.Dir, rel), "{}", old)
	}

	mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})

	for _, rel := range private {
		if _, err := os.Stat(filepath.Join(b.Dir, rel)); !os.IsNotExist(err) {
			t.Errorf("%s was copied", rel)
		}
	}
}

func TestSymlinksInSessionFoldersAreNotFollowed(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	secret := filepath.Join(root, "secret")
	writeFile(t, secret, "secret", old)
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", old)
	dir := filepath.Join(a.Dir, "projects", project, session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})

	if _, err := os.Lstat(filepath.Join(b.Dir, "projects", project, session, "link.txt")); !os.IsNotExist(err) {
		t.Errorf("symlink crossed accounts")
	}
}

func TestSidecarConflictTakesNewest(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	line := `{"uuid":"1"}` + "\n"
	writeFile(t, transcriptPath(a, project, session), line, old)
	writeFile(t, transcriptPath(b, project, session), line, old)
	title := filepath.Join("projects", project, session, "custom-title.json")
	writeFile(t, filepath.Join(a.Dir, title), "older!", old)
	writeFile(t, filepath.Join(b.Dir, title), "newer!", old.Add(time.Minute))

	results := mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})

	if results[0].Status != StatusUpdate {
		t.Errorf("status = %s, want update", results[0].Status)
	}
	if got := readFile(t, filepath.Join(a.Dir, title)); got != "newer!" {
		t.Errorf("sidecar = %q", got)
	}
}

func TestLinesLongerThanAnyScannerBuffer(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	huge := `{"uuid":"big","blob":"` + strings.Repeat("x", 3<<20) + `"}` + "\n"
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"root"}`+"\n"+huge, old)
	writeFile(t, transcriptPath(b, project, session), `{"uuid":"root"}`+"\n", old)

	mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})

	if !strings.Contains(readFile(t, transcriptPath(b, project, session)), huge) {
		t.Errorf("large line was not merged intact")
	}
}

func TestUnfinishedLastLineIsNeitherSpreadNorAllowedToCorruptAppends(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"a1"}`+"\n"+`{"uuid":"tru`, old)
	writeFile(t, transcriptPath(b, project, session), `{"uuid":"b1"}`+"\n", old)

	mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})

	gotA := readFile(t, transcriptPath(a, project, session))
	if !strings.Contains(gotA, `{"uuid":"tru`+"\n"+`{"uuid":"b1"}`+"\n") {
		t.Errorf("appended record was glued to the unfinished line: %q", gotA)
	}
	gotB := readFile(t, transcriptPath(b, project, session))
	if strings.Contains(gotB, "tru") {
		t.Errorf("unfinished line spread to another account: %q", gotB)
	}
	if !strings.Contains(gotB, `{"uuid":"a1"}`) {
		t.Errorf("complete line missing: %q", gotB)
	}

	if results := mustSync(t, Options{Accounts: []Account{a, b}, Apply: true}); results[0].Status != StatusInSync {
		t.Errorf("second run status = %s, want in-sync", results[0].Status)
	}
}

func TestNoTempFilesLeftBehind(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", old)

	mustSync(t, Options{Accounts: []Account{a, b}, Apply: true})

	for path := range snapshot(t, root) {
		if strings.Contains(path, tempPrefix) {
			t.Errorf("temp file left: %s", path)
		}
	}
}

func TestEncodeProjectMatchesClaudeCode(t *testing.T) {
	cases := map[string]string{
		"/home/brain-bytes/Codes/nstack/ccsync": "-home-brain-bytes-Codes-nstack-ccsync",
		"/home/brain-bytes/.cache/ccmeter":      "-home-brain-bytes--cache-ccmeter",
		"/tmp/claude-1000/-home-x/scratch_pad":  "-tmp-claude-1000--home-x-scratch-pad",
	}
	for in, want := range cases {
		if got := encodeProject(in); got != want {
			t.Errorf("encodeProject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoverAccounts(t *testing.T) {
	home := t.TempDir()
	newAccount(t, home, "work")
	writeFile(t, filepath.Join(home, ".claude.json"), "{}", old)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude-empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(custom, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A symlink to an account must not count as a second account.
	if err := os.Symlink(filepath.Join(home, ".claude-work"), filepath.Join(home, ".claude-alias")); err != nil {
		t.Fatal(err)
	}

	accounts, err := discoverAccounts(home, custom)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range accounts {
		names = append(names, a.Name)
	}
	if want := []string{"default", "elsewhere", "work"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestSelectAccounts(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	all := []Account{a, b}

	picked, err := selectAccounts(all, "b, b")
	if err != nil || len(picked) != 1 || picked[0].Name != "b" {
		t.Errorf("picked = %v, err = %v", picked, err)
	}
	if _, err := selectAccounts(all, "nope"); err == nil {
		t.Errorf("unknown account accepted")
	}
	if _, err := selectAccounts(all, root); err == nil {
		t.Errorf("path without projects/ accepted")
	}
}

func TestCommandOutput(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	// run uses the real clock, so the fixture has to be old relative to it.
	writeFile(t, transcriptPath(a, project, session),
		`{"type":"ai-title","aiTitle":"Generated"}`+"\n"+`{"type":"custom-title","customTitle":"Fix login"}`+"\n",
		time.Now().Add(-time.Hour))

	var out bytes.Buffer
	if err := run([]string{"status", "--all"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"copy", "a -> b", "Fix login", "1 would change"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output missing %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(transcriptPath(b, project, session)); !os.IsNotExist(err) {
		t.Errorf("status wrote a file")
	}

	out.Reset()
	if err := run([]string{"sync", "--all"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transcriptPath(b, project, session)); err != nil {
		t.Errorf("sync did not copy: %v", err)
	}

	out.Reset()
	if err := run([]string{"sync", "--project", "/no/such/project"}, &out); err == nil ||
		!strings.Contains(err.Error(), "no sessions") {
		t.Errorf("err = %v, want no-sessions error", err)
	}
	if err := run([]string{"sync", "--all", "--project", "/x"}, &out); err == nil {
		t.Errorf("--all with --project accepted")
	}
	if err := run([]string{"sync", "--all", "--accounts", "a"}, &out); err == nil {
		t.Errorf("single account accepted")
	}
}

func TestSessionScopeCoversOneSession(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	other := "22222222-2222-2222-2222-222222222222"
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", old)
	writeFile(t, transcriptPath(a, "-home-me-other", other), `{"uuid":"2"}`+"\n", old)

	results := mustSync(t, Options{Accounts: []Account{a, b}, Session: session, Apply: true})

	if len(results) != 1 || results[0].ID != session || results[0].Status != StatusCopy {
		t.Fatalf("results = %+v", results)
	}
	if _, err := os.Stat(transcriptPath(b, "-home-me-other", other)); !os.IsNotExist(err) {
		t.Errorf("unselected session was copied")
	}
}

func TestSessionPrefixIsResolvedOrRejected(t *testing.T) {
	root := t.TempDir()
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	other := "11222222-2222-2222-2222-222222222222"
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", old)
	writeFile(t, transcriptPath(a, project, other), `{"uuid":"2"}`+"\n", old)
	accounts := []Account{a, b}

	results := mustSync(t, Options{Accounts: accounts, Session: "1111", Apply: false})
	if len(results) != 1 || results[0].ID != session {
		t.Errorf("prefix results = %+v", results)
	}

	if _, err := Sync(Options{Accounts: accounts, Session: "11", Apply: false}); err == nil ||
		!strings.Contains(err.Error(), "matches several") {
		t.Errorf("err = %v, want ambiguity error", err)
	}
	if _, err := Sync(Options{Accounts: accounts, Session: "deadbeef", Apply: false}); err == nil ||
		!strings.Contains(err.Error(), "no session") {
		t.Errorf("err = %v, want not-found error", err)
	}
}

func TestSessionFlagSearchesEveryProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	a, b := newAccount(t, root, "a"), newAccount(t, root, "b")
	other := "22222222-2222-2222-2222-222222222222"
	recent := time.Now().Add(-time.Hour)
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", recent)
	writeFile(t, transcriptPath(a, "-home-me-other", other), `{"uuid":"2"}`+"\n", recent)

	var out bytes.Buffer
	if err := run([]string{"sync", "--session", session[:8]}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transcriptPath(b, project, session)); err != nil {
		t.Errorf("selected session was not copied: %v", err)
	}
	if _, err := os.Stat(transcriptPath(b, "-home-me-other", other)); !os.IsNotExist(err) {
		t.Errorf("unselected session was copied")
	}

	out.Reset()
	if err := run([]string{"sync", "--session", "deadbeef"}, &out); err == nil ||
		!strings.Contains(err.Error(), "no session") {
		t.Errorf("err = %v, want no-session error", err)
	}
}

func TestExcludedAccountsAreLeftAlone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	a, b, c := newAccount(t, root, "a"), newAccount(t, root, "b"), newAccount(t, root, "c")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", time.Now().Add(-time.Hour))

	var out bytes.Buffer
	if err := run([]string{"sync", "--all", "--exclude", "c"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transcriptPath(b, project, session)); err != nil {
		t.Errorf("selected account did not receive the session: %v", err)
	}
	if _, err := os.Stat(transcriptPath(c, project, session)); !os.IsNotExist(err) {
		t.Errorf("excluded account received the session")
	}

	out.Reset()
	if err := run([]string{"sync", "--all", "--exclude", "nope"}, &out); err == nil ||
		!strings.Contains(err.Error(), "unknown account") {
		t.Errorf("err = %v, want unknown-account error", err)
	}
	if err := run([]string{"sync", "--all", "--accounts", "a,b", "--exclude", "b"}, &out); err == nil ||
		!strings.Contains(err.Error(), "need at least two") {
		t.Errorf("err = %v, want too-few-accounts error", err)
	}
}

func TestExcludeCommandPersistsAcrossSyncs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	a, b, c := newAccount(t, root, "a"), newAccount(t, root, "b"), newAccount(t, root, "c")
	writeFile(t, transcriptPath(a, project, session), `{"uuid":"1"}`+"\n", time.Now().Add(-time.Hour))

	var out bytes.Buffer
	if err := run([]string{"exclude", "c"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Excluded from syncing: c") {
		t.Errorf("exclude output = %q", out.String())
	}

	out.Reset()
	if err := run([]string{"sync", "--all"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transcriptPath(b, project, session)); err != nil {
		t.Errorf("included account did not receive the session: %v", err)
	}
	if _, err := os.Stat(transcriptPath(c, project, session)); !os.IsNotExist(err) {
		t.Errorf("persistently excluded account received the session")
	}

	out.Reset()
	if err := run([]string{"exclude", "--remove", "c"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"sync", "--all"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transcriptPath(c, project, session)); err != nil {
		t.Errorf("account still excluded after removal: %v", err)
	}

	out.Reset()
	if err := run([]string{"exclude", "nope"}, &out); err == nil ||
		!strings.Contains(err.Error(), "unknown account") {
		t.Errorf("err = %v, want unknown-account error", err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
