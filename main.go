// Command ccsync keeps Claude Code project sessions in step across the config
// dirs (CLAUDE_CONFIG_DIR) of several accounts, so a session started in one
// account can be resumed from another.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `ccsync - share Claude Code project sessions between accounts

Usage:
  ccsync accounts                 List the config dirs that were found
  ccsync status [flags]           Show what a sync would do (changes nothing)
  ccsync sync   [flags]           Copy and merge sessions between accounts

Flags for status and sync:
  --project PATH     Project to cover (default: current directory)
  --all              Cover every project in every account
  --accounts LIST    Comma-separated account names or config dir paths
                     (default: all accounts found)

Accounts are ~/.claude* dirs containing projects/, plus $CLAUDE_CONFIG_DIR.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ccsync:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	discovered, err := discoverAccounts(home, os.Getenv("CLAUDE_CONFIG_DIR"))
	if err != nil {
		return err
	}

	switch args[0] {
	case "accounts":
		return printAccounts(out, discovered)
	case "status":
		return runSync(args[1:], discovered, false, out)
	case "sync":
		return runSync(args[1:], discovered, true, out)
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (try: ccsync help)", args[0])
}

func runSync(args []string, discovered []Account, apply bool, out io.Writer) error {
	flags := flag.NewFlagSet("ccsync", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", "", "")
	all := flags.Bool("all", false, "")
	accountSpec := flags.String("accounts", "", "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return nil
		}
		return fmt.Errorf("%w (try: ccsync help)", err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (try: ccsync help)", flags.Arg(0))
	}
	if *all && *project != "" {
		return fmt.Errorf("--all and --project cannot be combined")
	}

	accounts, err := selectAccounts(discovered, *accountSpec)
	if err != nil {
		return err
	}
	if len(accounts) < 2 {
		return fmt.Errorf("need at least two accounts to sync, have %d", len(accounts))
	}

	var projects []string
	var projectPath string
	if !*all {
		projectPath, err = resolveProject(*project)
		if err != nil {
			return err
		}
		projects = []string{encodeProject(projectPath)}
	}

	// Liveness covers every discovered account, not just the selected ones.
	liveSet := liveSessions(append(append([]Account{}, discovered...), accounts...))
	results, err := Sync(Options{
		Accounts: accounts,
		Projects: projects,
		Live:     liveSet,
		Apply:    apply,
		Now:      time.Now(),
	})
	if err != nil {
		return err
	}
	if len(results) == 0 && !*all {
		return fmt.Errorf("no sessions for %s in any account (use --project or --all)", projectPath)
	}
	return printResults(out, results, apply)
}

func resolveProject(path string) (string, error) {
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// A project that has been deleted can still have sessions to sync.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

func printAccounts(out io.Writer, accounts []Account) error {
	live := liveSessions(accounts)
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ACCOUNT\tSESSIONS\tCONFIG DIR")
	for _, account := range accounts {
		files, err := filepath.Glob(filepath.Join(account.Dir, "projects", "*", "*.jsonl"))
		if err != nil {
			return err
		}
		fmt.Fprintf(table, "%s\t%d\t%s\n", account.Name, len(files), account.Dir)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if len(live) > 0 {
		fmt.Fprintf(out, "\n%d running session(s) will be skipped until they exit.\n", len(live))
	}
	return nil
}

func printResults(out io.Writer, results []SessionResult, applied bool) error {
	counts := map[Status]int{}
	var failed []SessionResult
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	wrote := false
	for _, result := range results {
		if result.Err != nil {
			failed = append(failed, result)
			continue
		}
		counts[result.Status]++
		if result.Status == StatusInSync {
			continue
		}
		wrote = true
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", result.Status, shortID(result.ID), describe(result), displayTitle(result.Title))
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if wrote {
		fmt.Fprintln(out)
	}

	verb := "would change"
	if applied {
		verb = "changed"
	}
	changed := counts[StatusCopy] + counts[StatusMerge] + counts[StatusUpdate]
	fmt.Fprintf(out, "%d session(s): %d %s (%d copy, %d merge, %d update), %d in sync, %d skipped as running\n",
		len(results), changed, verb, counts[StatusCopy], counts[StatusMerge], counts[StatusUpdate], counts[StatusInSync], counts[StatusSkipped])

	for _, result := range failed {
		fmt.Fprintf(out, "error: %s: %v\n", result.ID, result.Err)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d session(s) failed", len(failed))
	}
	return nil
}

func describe(result SessionResult) string {
	switch result.Status {
	case StatusCopy:
		return strings.Join(result.From, ",") + " -> " + strings.Join(result.To, ",")
	case StatusSkipped:
		return "running"
	}
	return strings.Join(result.From, ",")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func displayTitle(title string) string {
	if title == "" {
		return "(untitled)"
	}
	if runes := []rune(title); len(runes) > 50 {
		return string(runes[:49]) + "…"
	}
	return title
}
