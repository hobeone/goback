# AGENTS.md — GoBack

Guidance for AI coding agents (Claude Code, Gemini, Codex, …) working in this repo.
`CLAUDE.md` imports this file; keep agent guidance here, not there.

## What it is

GoBack is a single-binary Go CLI that wraps `rsync` to make space-efficient,
versioned backups. Unchanged files are hard-linked to the previous snapshot
via `rsync --link-dest`, and old snapshots are pruned by a GFS
(daily / weekly / monthly) retention policy.

- **Language**: Go — `go.mod` declares `go 1.25.0`.
- **Runtime dependency**: `rsync` on `PATH`.
- **Libraries**: `gopkg.in/yaml.v3` (config), `github.com/rs/zerolog` (logging). Nothing else.
- **Layout**: everything lives in `package main`. `main.go` (~340 lines) holds all logic,
  `main_test.go` holds all tests. There are no subpackages.

## Commands

| Task | Command |
|---|---|
| Run | `go run . [-config config.yaml] [-dry-run]` |
| Build | `go build -o goback .` (`goback` is gitignored) |
| Test | `go test ./...` |
| Vet | `go vet ./...` |
| Lint | `golangci-lint run` |
| Format | `gofmt -l .` must print nothing |

`config.yaml` in the repo root is an **example** that points at `/mnt/backups`.
Don't run the binary against it without `-dry-run` unless you mean it.

### Current gate status (as of 2026-11-16)

`go test`, `go vet` and `gofmt` are clean. `golangci-lint run` reports one known
issue: `errcheck` on `defer logFile.Close()` in `runRsync` (`main.go`). Closing a
file that was written to can fail, so fix it properly rather than suppressing it.

There is no CI workflow. `.github/` only has Dependabot (weekly `gomod`), so nothing
enforces these gates. Run them by hand.

## Architecture

`main()` parses flags, sets up a zerolog `ConsoleWriter`, reads config, and builds an
`App{config, dryRun, execCmd}`. `App.Run()` dispatches on `config.Mode`:

### Snapshot mode (`mode: snapshot` or unset)

1. `runSnapshotBackup`
   - `os.RemoveAll(<dest>/.unfinished)`, then recreate it. A previous partial run is
     **discarded**, not resumed.
   - `getLatestSnapshot` → newest snapshot dir becomes `--link-dest`.
   - `runRsync` into `.unfinished`. Stdout goes to `.unfinished/rsync.log`; stderr goes
     to both the console and the log.
   - `os.Rename(.unfinished, <dest>/<prefix>_<2006-01-02_15:04:05>)`. The rename is the
     commit point: a snapshot either exists in full or not at all.
2. `purgeBackups` (runs only if the backup succeeded)
   - Walks snapshots **newest-first** in three passes: daily (the N newest), weekly (the
     newest per ISO week), and monthly (the newest per calendar month).
   - Snapshots already kept by an earlier pass still claim their week/month bucket but
     **don't use up** a slot in the later pass. So `keep: {daily: 2, weekly: 2}` can
     keep 4 snapshots.
   - Deletion errors are logged, not returned. Purge never fails the run.

### Simple mode (`mode: simple`)

`MkdirAll(destination)`, then `rsync --delete` straight into it. No versioning, no purge,
and rsync output goes to stdout.

### rsync invocation (`runRsync`)

The fixed base is `-a -v -h --delete --stats --inplace`. Then come `--link-dest=…`,
one `--exclude=…` per entry, the extra flags, sources, and dest. Specifically:

- `rsync_extra_flags` is split with `strings.Fields`. **There is no shell quoting**, so a
  flag value containing spaces can't be expressed.
- `-dry-run` appends rsync's `--dry-run` (unless `-n` / `--dry-run` is already present)
  and sends rsync output to the console instead of a log file.
- Exit code 24 (files vanished during transfer) becomes a warning only when
  `ignore_vanished_files_error: true`.

### Snapshot discovery (`getSnapshots`)

A snapshot is **any non-dot directory directly under `destination`**, sorted by
**directory mtime**. Two consequences matter for any change to this code:

1. **`snapshot_prefix` is not a filter.** Any other directory placed in `destination`
   counts as a snapshot and can be **deleted by purge**.
2. **Order comes from mtime, not from the timestamp in the name.** See *Known issues*.

## Configuration

| Key | Notes |
|---|---|
| `mode` | `snapshot` (default when empty) or `simple`; anything else is an error |
| `destination` | Root for snapshots (snapshot mode) or the sync target (simple mode) |
| `snapshot_prefix` | Name prefix only; not used to identify snapshots |
| `source` | List of paths passed to rsync as given (trailing slashes matter to rsync) |
| `exclude` | Each entry becomes `--exclude=<entry>` |
| `keep.daily/weekly/monthly` | Ints; **all zero means purge deletes every snapshot**, including the one just made |
| `rsync_extra_flags` | Whitespace-split string |
| `ignore_vanished_files_error` | Treat rsync exit 24 as success |

Config is never validated: an empty `destination` or `source`, or a missing `keep`
block, goes straight to rsync or purge.

## Known issues and hazards

These were confirmed on 2026-11-16. Fix them deliberately, with tests. Don't
"tidy" around them.

1. **mtime ordering can be corrupted by rsync.** With a trailing-slash source (`/data/`),
   `rsync -a` copies the source directory's mtime onto `.unfinished`. The new snapshot
   can then look *older* than existing ones, which breaks both `--link-dest` selection
   and retention. Verified: `rsync -a src/ d1` gave `d1` the mtime of `src`. Parsing
   the timestamp from the snapshot name would be more robust.
2. **Purge can delete things that aren't snapshots** (see *Snapshot discovery*).
3. **An all-zero or missing `keep` block deletes everything.**
4. **Snapshot names contain `:`**, which won't work on SMB/exFAT/Windows-backed destinations.

## Testing conventions

- **The rsync mock** uses the stdlib helper-process pattern. `App.execCmd` is
  `exec.Command` in prod; tests inject `mockExecCommand`, which re-executes the test
  binary into `TestHelperProcess` with `GO_WANT_HELPER_PROCESS=1`. Today the helper
  **always exits 24** for `rsync`. That's why `TestRunSimpleBackup` sets
  `IgnoreVanishedFilesError: true`. To simulate other outcomes, have the helper choose
  its behavior from an env var or from the args.
- **Gaps**: no test asserts the rsync argv (excludes, extra flags, dry-run injection,
  `--link-dest`); no test covers `App.Run` mode dispatch, the rename/commit step, the
  dry-run paths, or `getSnapshots` filtering.
- **Purge tests** create directories and set their mtimes with `os.Chtimes`. Use
  **fixed dates** (the `day()` helper, which returns noon local time), never offsets from
  `time.Now()`. Offsets make week and month bucketing depend on when the test runs.
  Purge itself never reads the clock.
- Use `t.TempDir()` for every temp path. The `writeConfig` and `newSourceDir` helpers
  cover the common setup.
- New behavior needs a test. Before trusting a new test, confirm it fails without the
  fix.

## Style and conventions

- Standard Go idioms. Wrap errors with `fmt.Errorf("…: %w", err)`. Only `main()` calls
  `log.Fatal`.
- Every side-effecting step checks `a.dryRun` and logs a `[dry run] would …` line
  instead. Keep that symmetry when you add steps.
- Logging is zerolog with structured fields (`.Str("path", …)`); messages are lowercase.
- Commits follow Conventional Commits (`feat:`, `fix(purge):`, `docs:`, …).

## Docs drift to watch

`README.md` still says Go 1.18+. It doesn't document `mode` or
`ignore_vanished_files_error`, and it doesn't mention the hazards above. When you
change behavior, update README.md and this file together.
