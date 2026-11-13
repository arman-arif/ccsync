# ccsession

Share Claude Code project sessions between accounts on one machine, so a session
started under one `CLAUDE_CONFIG_DIR` can be resumed from another.

## Install

```sh
go install github.com/arman-arif/ccsession@latest
```

Or from a clone:

```sh
make build
make install PREFIX=~/.local   # or: sudo make install
```

Run `make help` for the other targets.

Needs Go 1.24+ and Linux or macOS. It has no dependencies outside the standard library.

## Usage

```sh
ccsession accounts                        # list the config dirs found
ccsession exclude work                    # skip an account in every sync
ccsession exclude --remove work           # start syncing it again
ccsession status                          # preview for the current project
ccsession sync                            # sync the current project
ccsession sync --project ~/Codes/app      # sync another project
ccsession sync --session 1a2b3c4d         # only that session (prefix is fine)
ccsession sync --accounts work,personal   # limit which accounts take part
ccsession sync --exclude work             # leave an account out of one run
ccsession sync --all                      # every project in every account
ccsession sync --force                    # include sessions that look running
```

Accounts are the `~/.claude*` dirs containing `projects/`, plus
`$CLAUDE_CONFIG_DIR`. `~/.claude` is named `default` and `~/.claude-work` is
named `work`. `--accounts` also accepts full paths.

`--session` takes a session ID or an unambiguous prefix and searches every
project unless `--project` is given. Pass a longer prefix if it reports that
the ID matches several sessions.

`--exclude` takes the same names or paths as `--accounts` and removes those
accounts after `--accounts` has selected the ones to sync.

`ccsession exclude ACCOUNT...` makes the exclusion permanent. The list is stored
in `$XDG_CONFIG_HOME/ccsession/config.json` (default
`~/.config/ccsession/config.json`) and is read by every `status` and `sync`.
Entries are account names; an entry whose account no longer exists is ignored.
Excluded accounts stay out even when named in `--accounts`. Run `ccsession
exclude` with no arguments to show the list, or `ccsession exclude --remove
ACCOUNT...` to take an account off it.

After syncing, resume from the other account as usual:

```sh
CLAUDE_CONFIG_DIR=~/.claude-work claude --resume
```

`--all` gives every account a copy of every session, which is usually not what
you want if you keep work and personal accounts separate. A resumed transcript
is sent to the account that resumes it, so sync only what that account may see.

## What gets synced

| Synced for each session | How |
|---|---|
| `projects/<project>/<id>.jsonl` | Lines are merged |
| `projects/<project>/<id>/subagents/*.jsonl` | Lines are merged |
| Other files in `projects/<project>/<id>/` | Newest wins |
| `file-history/<id>/` (for `/rewind`) | Newest wins |

Nothing else is touched: credentials, `.claude.json`, settings, `history.jsonl`,
`session-env`, `sessions/` and project `memory/` all stay per-account.

## How it behaves

- **Merging never loses lines.** If a session was continued in two accounts
  between syncs, each copy gains the lines it lacks. Existing lines are not
  rewritten, and lines are compared as raw bytes, so this doesn't rely on the
  undocumented transcript schema. Damaged lines are not shared.
- **Running sessions are skipped.** That means sessions whose process is listed
  in any account's `sessions/` registry, or transcripts modified in the last 30
  seconds. Run `ccsession sync` again after they exit, or pass `--force` to sync
  them anyway. The other accounts then get a snapshot that misses whatever the
  session writes afterwards (the next sync picks that up), and lines from other
  accounts are appended to a transcript the session is still writing.
- **New files appear atomically** and keep their original mtime and permissions.
- **Deletions don't propagate.** A session deleted or cleaned up in one account
  comes back on the next sync while another account still has it.

## Known limits

- **Forked sessions.** Each account's copy has the same lines in a different
  order, and Claude Code's choice of branch after a fork isn't confirmed. Both
  branches stay in the file, and `sync` reports these sessions as `merge`.
- **Account-tagged records.** Transcripts contain `session-owner` and
  `bridge-session` records naming the originating account, and signed thinking
  blocks. They are copied as they are, because changing them would stop merged
  copies matching.
- **Paths in old messages.** Older tool output may mention the original account's
  config dir. Those paths are not rewritten, and they still exist on this machine.
- **One machine only.** Project folder names come from absolute paths, so ccsession
  does not sync between computers.
