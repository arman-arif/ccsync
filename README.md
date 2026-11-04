# ccsync

Share Claude Code project sessions between accounts on one machine, so a session
started under one `CLAUDE_CONFIG_DIR` can be resumed from another.

## Install

```sh
go install github.com/arman-arif/ccsync@latest   # once the repo is pushed
go build -o ccsync .                             # or from this folder
```

Needs Go 1.24+ and Linux or macOS. It has no dependencies outside the standard library.

## Usage

```sh
ccsync accounts                        # list the config dirs found
ccsync status                          # preview for the current project
ccsync sync                            # sync the current project
ccsync sync --project ~/Codes/app      # sync another project
ccsync sync --accounts work,personal   # limit which accounts take part
ccsync sync --all                      # every project in every account
```

Accounts are the `~/.claude*` dirs containing `projects/`, plus
`$CLAUDE_CONFIG_DIR`. `~/.claude` is named `default` and `~/.claude-work` is
named `work`. `--accounts` also accepts full paths.

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
  seconds. Run `ccsync sync` again after they exit.
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
- **One machine only.** Project folder names come from absolute paths, so ccsync
  does not sync between computers.
