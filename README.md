# ccvault

Archive and search your Claude Code conversation history.

Inspired by [msgvault](https://github.com/wesm/msgvault), ccvault provides offline search, analytics, and AI integration for Claude Code sessions stored in `~/.claude`.

<img width="557" height="563" alt="image" src="https://github.com/user-attachments/assets/5ce5ecee-f3eb-4a93-8685-c7b138ea08dd" />


## Features

- **Full-text search** across all conversations with Gmail-like query syntax
- **Interactive TUI** for browsing and analyzing sessions
- **DuckDB analytics** for fast aggregate queries over Parquet exports
- **MCP server** for AI assistant integration

## Installation

### Homebrew (macOS)

```bash
brew install 2389-research/tap/ccvault
```

The tap carries macOS builds only — on Linux use `go install` or build from
source.

### Using `go install`

Requires Go 1.25 or later:

```bash
go install github.com/2389-research/ccvault/cmd/ccvault@latest
```

Make sure `$GOPATH/bin` (or `$HOME/go/bin`) is in your `PATH`.

### Build from source

```bash
git clone https://github.com/2389-research/ccvault.git
cd ccvault
make build
sudo mv ccvault /usr/local/bin/
```

`make build` writes `./ccvault` and stamps it with the version, commit and
build date from your checkout. A plain `go build ./cmd/ccvault` works too, but
the binary then reports its version as `dev`.

### Verify installation

```bash
ccvault version
```

Which prints the version, the commit it was built from, and when:

```
ccvault 0.3.0
commit: 8a3df3533c167834ae79fddc557f0268610604bd
built:  2026-10-08T12:34:56Z
```

The first line is always `ccvault <version>`, so a script can read the version
off it. An unstamped source-tree build reports `dev` with the commit Go embeds
from the tree, while one installed with `go install <module>@<version>` reports
that module version. A fact the binary does not carry reads `unknown` rather
than being guessed at.

## Quick Start

```bash
# Sync conversations from ~/.claude
ccvault sync

# Launch interactive TUI
ccvault tui

# Search conversations
ccvault search "debugging async"
ccvault search "project:myapp model:opus"

# View statistics
ccvault stats
```

## Commands

| Command | Description |
|---------|-------------|
| `quickstart` | Interactive setup guide for new users |
| `orient` | Database state summary for AI agents (use `--json`) |
| `sync` | Sync conversations from Claude Code (see [Sync modes](#sync-modes)) |
| `import [db-path]` | Merge another ccvault database into this archive |
| `repair-index` | Rebuild the full-text search indexes from the archive's own rows |
| `repair-tokens` | Re-derive the prompt-cache token counters of sessions that read zero |
| `tui` | Launch interactive terminal UI |
| `search [query]` | Full-text search across conversations |
| `stats` | Show archive statistics, including reclaimable space |
| `vacuum` | Reclaim dead space in the database file (see [Storage reclaim](#storage-reclaim)) |
| `list-projects` | List all indexed projects |
| `list-sessions` | List top-level sessions (optionally filtered by project; `--include-subagents` / `--subagents-of <id>` expand subagent transcripts) |
| `show [session-id]` | Display a specific session |
| `export [session-id]` | Export a session to markdown |
| `build-cache` | Build Parquet analytics cache |
| `mcp` | Start MCP server for AI integration |
| `schedule` | Register an hourly background sync and report on it (see [Scheduled sync](#scheduled-sync)) |
| `version` | Print the version number |

## Sync modes

ccvault is an archive, not a cache of what Claude Code currently has on disk.
Claude Code prunes its own session files, so the archive is usually the only
place older conversations still exist. The sync modes differ in whether they
respect that.

| Mode | What it does | Destroys pruned history? |
|------|--------------|--------------------------|
| `ccvault sync` | Re-parses only files whose mtime changed | No |
| `ccvault sync --full` | Re-parses every discovered file, ignoring mtimes. Each file replaces its own rows; rows whose source file is gone are left alone | No |
| `ccvault sync --rebuild` | Wipes every table, then re-scans, so the archive holds exactly what is on disk now | **Yes** |

Reach for `--full` after a parser change that extracts more from the same
JSONL. Reach for `--rebuild` only when you actually want the archive reduced
to current disk state; it prompts for confirmation, reports how many sessions
have no source file left (those are gone for good), and takes a `VACUUM INTO`
backup to `~/.ccvault/backups/` first.

To recover from a rebuild — or to consolidate two machines' archives — merge a
database back in:

```bash
ccvault import ~/.ccvault/backups/ccvault-20260905-140418.db
```

`import` only adds. Sessions absent locally are inserted with their turns and
tool uses; a session present in both is kept as-is unless the incoming copy
ended later. The source database is only read, and the whole merge is one
transaction.

A source written by a newer ccvault than the one you are running is refused.
Reading it cannot damage it; the risk is reading columns whose meaning has
changed — migration 012 re-keyed `turns` without renaming a column, which a
column check cannot catch — and writing the misreading into this archive.
Importing from a newer archive is legitimate, so `--allow-newer-schema` goes
ahead anyway.

### One-time counter correction

`projects.session_count` and `total_tokens` were maintained additively, so
every re-parse of an already-indexed session added to them again. Sync now
recomputes both from the `sessions` rows that actually exist.

The first sync after upgrading therefore **corrects these two numbers
downward**, sometimes by a lot — an archive synced many times may show project
token counts an order of magnitude above the real figure. Nothing is deleted
and no session, turn, or tool use is affected; only the two display counters
on `projects` change, and they change to the truth. `first_seen_at` and
`last_activity_at` are left alone.

## Storage reclaim

Incremental sync replaces the rows of every changed session file. SQLite keeps
the pages it frees on an internal freelist instead of returning them to the
filesystem, so the database file grows away from the data it holds. A
long-running archive can end up majority dead space — one real archive measured
4.6 GB on disk holding 1.9 GB of data.

`ccvault stats` and `ccvault orient --json` report the condition:

```
Storage:
  Database file: 4.6 GB
  Reclaimable:   2.9 GB (60% of the file)
  Reclaim it with 'ccvault vacuum'.
```

`sync` prints the same one-line note when the waste crosses 25% of the file and
32 MB. Nothing is reclaimed automatically — compaction rewrites the whole file
and needs the database to itself, which is no business of a sync run.

`ccvault vacuum` reclaims it:

```bash
ccvault vacuum          # human-readable progress and before/after figures
ccvault vacuum --json   # same numbers for machine consumption
```

It writes a compacted copy beside the live database with `VACUUM INTO`, checks
the copy with `integrity_check`, a row count per table and FTS5's own strict
integrity-check on every search index, and renames it over the original in one
atomic step. The original is never modified, so any failure leaves it exactly
as it was. Consequences worth knowing:

- It needs free disk space for the compacted copy — roughly the size of the
  live data, not of the whole file. It refuses up front if the space isn't
  there, rather than failing partway through a multi-gigabyte write.
- It takes the database exclusively. Close the TUI, MCP server, and any running
  sync first; it refuses rather than compacting a file someone else is writing.
- It rewrites the whole file. Expect tens of seconds on a multi-gigabyte
  archive (4.6 GB compacted to 1.9 GB in about 21 seconds).
- A WAL-mode archive stays in WAL mode. `VACUUM INTO` always writes a
  rollback-journal database, so the mode is restored after the swap; and the
  write-ahead log is only removed once SQLite confirms every frame of it was
  folded into the main file.
- It refuses to swap in a copy whose search index is malformed. Compaction is
  the one ccvault operation a bad outcome cannot be undone by re-running, so a
  copy that fails FTS5's own check is discarded and the original kept. Repair
  the index with `ccvault repair-index` and run it again.

### Verifying the search index

`ccvault stats` reports how many documents the search index holds. That number
is a lower bound, not a verdict: a stranded document is only visible in it while
its rowid is unused, and a re-parse hands every turn a fresh rowid, so the
counts agree again while the index still answers queries with the stale terms.
Measured: 2,000 stranded documents in a 235,101-turn archive read as zero
orphans while FTS5 called the index malformed.

```bash
ccvault stats --strict   # FTS5's own integrity-check, plus the timestamp-layout count
```

That is the only thing that returns a verdict. It is opt-in because it
re-tokenizes the archive — about 60% of the cost of a full rebuild, so a couple
of seconds on a million turns — and it exits non-zero if any index fails, so a
script or an agent cannot read a pass off a failing run. Repair with
`ccvault repair-index`.

### Repairing the search index

```bash
ccvault repair-index
```

The indexes are FTS5 external-content tables, so each one is a pure function of
the table it indexes: rebuilding reads nothing off disk and changes nothing but
the index. Measured on a 1,049,544-turn archive whose `turns_fts` held 136,279
stranded documents — about 20 seconds for that index, and the strict check clean
afterwards. `ccvault sync --full` also rebuilds them, but it re-reads every
transcript on disk to do it: tens of thousands of files, to repair a structure
derived entirely from rows already in the archive.

It runs no migrations, deliberately. An index malformed badly enough aborts
migration 012, which ends in FTS5's integrity-check — and because every other
command migrates the archive as it opens it, the damage otherwise stands in
front of its own repair. It exits non-zero if an index is still malformed after
the rebuild, which means the damage is in the content tables and those do need
re-reading with `ccvault sync --full`.

`--strict` also counts how many values in the archive's `DATETIME` columns are
outside the layout ccvault writes (RFC3339, UTC, nine fraction digits). That
layout is load-bearing: `ORDER BY`, `MIN()`/`MAX()` and the date filter all
compare the stored text, and the generated `search_period` column slices the
calendar date out of it by character position, so a value in a different shape
sorts and indexes wherever its bytes land. Such a value is still readable, and
an import could legitimately carry one, so this is reported rather than
refused — it does not change the exit code.

### Repairing the cache token counters

```bash
ccvault repair-tokens --dry-run   # what it would repair
ccvault repair-tokens             # repair it
```

`sessions.cache_read_tokens` and `cache_write_tokens` were never assigned, so
every session synced before that fix reads zero on both — 48,064 of 49,226
sessions on the author's archive. Measured there: the repair recovered 23.1
billion cache tokens, 150 times the 154 million input and output tokens those
sessions counted, taking the archive's reported total from 6.3 to 29.4 billion
in under two minutes. `ccvault stats` reports how many sessions are in that
state, and says nothing when there are none.

`ccvault sync` repairs a session it re-reads, but only a few hundred of those
transcripts still exist under `~/.claude`, so for the rest there is nothing on
disk left to read. `repair-tokens` reads the archive instead: `turns.raw_json`
holds the complete JSONL line of every turn, so the lines are streamed back
through the same adapter that first parsed them and summed the way a live sync
sums them. That keeps one implementation of the per-source rule, which matters
because the rule is not uniform — codex reports a cached count contained inside
its input count, where Claude Code reports cache counters beside it, so one is
a split and the other a copy.

It refuses rather than guessing. A session whose re-derived input and output
tokens disagree with the ones the row already counts is left alone: those two
were counted correctly from the start, so re-deriving them is a checksum over
the bytes the repair is about to trust. A session holding a turn with no stored
line at all cannot be re-derived and is skipped. Both refusals are counted and
named, and the mismatches are broken down by source — every pre-fix codex
session is permanently among them, because codex reports its token counts on
event lines that never became turns and so never reached `raw_json`.

One case is reported rather than refused. A turn whose line was too large to
store is kept as a placeholder without its usage, so a session holding one
re-derives short by that turn. Refusing it would leave the whole session at
zero to avoid being short by one turn — on the author's archive, 7 such turns
across 6 sessions that hold 1.29 billion cache tokens between them — so those
sessions are repaired and the caveat is printed against them.

Safe to re-run and to interrupt: a repaired session is no longer a candidate,
progress is committed as it goes, and each commit runs on a context the
cancellation cannot reach, so Ctrl-C keeps what it had already derived. Run
`ccvault build-cache` afterwards to carry the repair into the analytics cache.

### Running an older ccvault against a newer archive

ccvault refuses to write an archive whose schema version is higher than the
migrations it ships, and names the binary and path it is running from. An old
binary that still keyed turns on the uuid alone deleted 35,090 turns from a
migrated archive before this guard existed. Reads are allowed and print a
warning: a newer schema is normally still readable, but the state — an old
binary pointed at a new archive — is the one that precedes the write.

## Scheduled sync

ccvault only archives what it has seen, and Claude Code keeps transcripts on
disk for roughly 38 days. A month of not running `sync` is a month that cannot
be recovered afterwards. `ccvault schedule` makes archive completeness stop
depending on remembering.

```bash
ccvault schedule install     # register an hourly `ccvault sync`
ccvault schedule status      # report what is registered and whether it works
ccvault schedule uninstall   # remove it
```

The job is a **user-level unit and never needs `sudo`**. It runs plain
`ccvault sync` and never `sync --full` or `sync --rebuild` — both are expensive
or destructive and stay human-initiated. Installing twice leaves one unit.

The first run is one interval after install, not immediately; `install` says so
rather than leaving you wondering. Run `ccvault sync` yourself if you want the
archive current today.

### Platform support

| Platform | Mechanism | State |
|---|---|---|
| macOS | launchd `LaunchAgent` in `gui/<uid>` | Implemented, **verified on a real Mac** |
| Linux | systemd `--user` timer | Implemented, **registration unverified** — see below |
| Windows, everything else | — | **Not implemented.** The commands say so |

The Linux caveat is specific and worth stating plainly. The unit files
themselves are generated by a pure function and tested byte for byte, and the
`systemctl --user` argv, the output parsing and the post-install verification
are all tested too. What has never been exercised is a real `systemctl --user`
on a real Linux box — `systemd --user` needs a session bus, which a CI runner
does not have. `install` re-inspects after registering and fails if the timer
is not active, so it cannot report success for a job that does not exist; but
"has not been seen to work" is not the same as "works".

Windows is not implemented on purpose. `schtasks` could be driven and the task
XML would be no harder to generate than the plist, but registration is the half
tests cannot stand in for. A command whose success message has never been
observed to be true is worse than one that refuses.

### `schedule status`

`status` is the part worth running. It reports **reality, not intent** — every
fact read from the system, never from a config file:

```
$ ccvault schedule status
ccvault Scheduled Sync
======================

Mechanism:     launchd (user-level, no sudo)
Label:         ai.2389.ccvault.sync
Registered:    yes
Unit:          /Users/you/Library/LaunchAgents/ai.2389.ccvault.sync.plist
Runs:          /Users/you/go/bin/ccvault sync
Every:         1h0m0s
Binary exists: yes
Matches PATH:  yes
Last run:      2026-10-29 15:20, exited 0
Run count:     12
Output:        /Users/you/.ccvault/schedule.log
Archive:       last advanced 2026-10-29 15:20 (31m ago)

Everything checks out.
```

Four independently checked facts:

1. **Is a unit actually registered with the OS** — asked of `launchctl` /
   `systemctl`, not inferred from a file on disk.
2. **Does the registered binary still exist, and is it still the `ccvault` on
   your `PATH`** — compared with symlinks resolved on both sides, so a Homebrew
   install is not reported as a mismatch forever.
3. **When did it last run, and did it succeed.** "Has never run" never renders
   as "exited 0".
4. **How far behind the archive is** — from `source_files.synced_at`, not from
   the timer. An hourly timer firing into a binary that refuses to write looks
   perfect from the scheduler's side.

Fact 2 is the one that bites, and it is why there is no shim. A unit hardcodes
a program path, and some install routes move: a `GOBIN` under `mise` embeds the
Go version, so bumping Go leaves the unit running a `ccvault` that no longer
matches the archive. That binary refuses to write rather than corrupt anything
— and a background job's refusal has no audience unless something reports it.

`status` exits non-zero when anything is wrong, including when nothing is
registered, so this does the right thing in a script:

```bash
ccvault schedule status || ccvault schedule install
```

`--json` emits the same facts with stable problem codes (`not-registered`,
`binary-missing`, `binary-mismatch`, `no-ccvault-on-path`, `last-run-failed`,
`archive-stale`).

### Running more than one

`--label` names the unit. Give the same value to all three subcommands; it is
the key `install` is idempotent on. Use it to schedule a second archive, or to
exercise the commands without touching a registration you depend on:

```bash
ccvault --data-dir /mnt/big schedule install --label ccvault-sync-big
```

An explicit `--data-dir`, `--config` or `--claude-home` is carried into the
registered command, so the unit syncs the archive you were looking at rather
than a different one. A default install registers plain `ccvault sync` and
nothing more.

### The log

Under launchd the job appends stdout and stderr to `<data_dir>/schedule.log`,
which `status` names. It grows by a few hundred bytes a run — under 2 MB a year
— and is not rotated. Under systemd the output goes to the journal
(`journalctl --user -u <label>.service`).

## Search Syntax

ccvault supports Gmail-like query syntax:

```
project:name     Filter by project path/name
model:opus       Filter by model (partial match)
tool:Bash        Sessions using specific tool
file:path        Filter by file path
before:date      Turns before date, excluding that date itself (YYYY-MM-DD)
after:date       Turns from the start of date, including that date
has:error        Sessions with errors
has:subagent     Sessions with subagent usage
"exact phrase"   Exact phrase match
```

The two date filters are not symmetric, which is worth knowing before reading
a result count. `before:` stops at the named date without reaching it, so
`after:2026-01-01 before:2026-01-31` covers the 1st through the 30th and not
the 31st. To include a day, name the one after it: `before:2026-02-01`.

Every date is read on your own calendar. A written-out date names local
midnight of that day, and the relative forms — `today`, `yesterday`, `week`,
`month` — are resolved the same way, so `after:today` and `after:2026-01-15`
both mean the day it is where you are. Turns are stamped in UTC; the filter
converts your bound to UTC before comparing, rather than comparing the two
wall clocks against each other.

Examples:
```bash
ccvault search "debugging the API endpoint"
ccvault search "project:myapp after:2024-01-01"
ccvault search "tool:Edit model:opus"
ccvault search '"error handling" project:backend'
```

## MCP Server

ccvault includes an MCP (Model Context Protocol) server for AI assistant integration.

```bash
ccvault mcp
```

Available tools:
- `search_conversations` - Full-text search across conversations
- `get_session_summary` - Quick overview of a session (metadata, stats, tools used)
- `get_turns` - Paginated turns from a session
- `get_session` - Full session in markdown format
- `list_sessions` - List recent sessions
- `list_projects` - List all indexed projects
- `get_stats` - Archive statistics
- `get_analytics` - Detailed usage analytics

### Claude Desktop Configuration

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "ccvault": {
      "command": "ccvault",
      "args": ["mcp"]
    }
  }
}
```

## Claude Code Skill

ccvault ships with a [Claude Code skill](skills/ccvault/SKILL.md) that teaches AI agents how to effectively mine conversation history. It includes search strategy patterns, workflow prompts for session orientation and on-demand recall, and a full tool/query reference card.

See [`skills/ccvault/`](skills/ccvault/) for the full skill.

## Configuration

ccvault uses sensible defaults but can be configured via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `CCVAULT_CLAUDE_HOME` | `~/.claude` | Claude Code data directory |
| `CCVAULT_DATA_DIR` | `~/.ccvault` | ccvault data directory |
| `GLAMOUR_STYLE` | `dark` | Markdown colour scheme the TUI renders conversations with |

### Markdown colours in the TUI (`GLAMOUR_STYLE`)

The TUI renders conversation Markdown with [glamour](https://github.com/charmbracelet/glamour),
and **defaults to the dark colour scheme regardless of your terminal's actual
background**. On a light-background terminal, set:

```bash
export GLAMOUR_STYLE=light
```

The value is glamour's own: a builtin style name (`dark`, `light`, `dracula`,
`notty`, `ascii`, …) or a path to a style JSON file. ccvault reads the same
variable glamour does, so a setting you already have for other glamour-based
tools is picked up as-is. There is deliberately no `config.toml` equivalent.

Why this isn't auto-detected: asking the terminal for its background colour
means an OSC 11 query, and the library that answers it waits up to five seconds
*per byte* of the reply. Over mosh or tmux the reply can simply never arrive —
the archive owner saw 60+ seconds of blank screen before the dashboard drew its
first frame. A wrong-but-instant colour scheme beats a correct one that hangs,
so the query is gone and the style comes from this variable instead.

### Pointing ccvault at another archive

Three persistent flags are available on every command:

| Flag | Answers |
|------|---------|
| `--data-dir <path>` | Where the database lives |
| `--claude-home <path>` | Where conversations are read from |
| `--config <path>` | Which config file to read |

`data_dir` is resolved highest precedence first:

1. `--data-dir`
2. `CCVAULT_DATA_DIR`
3. `data_dir = "..."` in the config file
4. `~/.ccvault`

`claude_home` follows the identical chain: `--claude-home`,
`CCVAULT_CLAUDE_HOME`, `claude_home = "..."`, `~/.claude`.

`--data-dir` also **replaces** the config search path, so `~/.ccvault` is not
read at all when it is given.

**`--data-dir` alone is enough to isolate a run.** When it is given and nothing
says where conversations come from, the source tree is inferred as
`<data-dir>/claude` rather than falling through to the real `~/.claude`:

```bash
mkdir -p /tmp/fixture/claude
ccvault --data-dir /tmp/fixture sync        # reads /tmp/fixture/claude
ccvault --data-dir /tmp/fixture stats --json
```

Naming a source turns the inference off, which is how you relocate the archive
while still indexing the real transcripts:

```bash
ccvault --data-dir /mnt/big --claude-home ~/.claude sync
```

`claude_home` or a `[[sources]]` list in the config file does the same. A
*durable* relocation — `CCVAULT_DATA_DIR`, or `data_dir` in the config file —
infers nothing and keeps reading the real source; only the ad-hoc flag, the
form a test or sandbox types, implies isolation.

If the inferred tree does not exist, `sync` refuses and names it, rather than
exiting 0 having indexed nothing.

`--claude-home` also **replaces** any `[[sources]]` list the config file
declares, collapsing it to the single claude-code tree named on the flag. A
partial override would leave a configured source pointing at a tree you did
not name, which is the hole the flag exists to close. A config file that
declares both `claude_home` and `[[sources]]` is hand-authored and keeps its
list — only the flag and `CCVAULT_CLAUDE_HOME` replace it.

`--config` names a single file instead of searching for one, and a path that
does not exist is an error rather than a silent fall-back to defaults. Given
both, `--config` decides where settings come from and `--data-dir` still wins
for `data_dir` itself. A config file that exists but cannot be parsed is an
error for the same reason: silently falling back to defaults would send a
fixture run at the real `~/.claude`.

## Data Storage

- **SQLite database**: `~/.ccvault/ccvault.db` - Session data with FTS5 full-text search
- **Analytics cache**: `~/.ccvault/analytics/sessions.parquet` - Parquet export for DuckDB queries

## Architecture

```
ccvault/
├── cmd/ccvault/     # CLI entry point
├── pkg/
│   ├── models/      # Data structures
│   └── parser/      # JSONL session parser
└── internal/
    ├── config/      # Configuration
    ├── db/          # SQLite layer with FTS5
    ├── sync/        # Incremental sync logic
    ├── search/      # Query parsing and execution
    ├── export/      # Markdown export
    ├── tui/         # Bubble Tea terminal UI
    ├── analytics/   # DuckDB/Parquet export
    └── mcp/         # MCP server
```

## License

MIT
