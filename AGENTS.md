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
- **Layout**: everything lives in `package main`. `main.go` holds all logic,
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

### Gate status

All four gates (`go test`, `go vet`, `gofmt`, `golangci-lint`) must stay clean. A new
failure is yours.

There is no CI workflow. `.github/` only has Dependabot (weekly `gomod`), so nothing
enforces these gates. Run them by hand.

## Architecture

`main()` parses flags, sets up a zerolog `ConsoleWriter`, reads config, and builds an
`App{config, dryRun, execCmd}`. `App.Run()` first calls `Config.validate()` (see
*Configuration*), then dispatches on `config.Mode`:

### Snapshot mode (`mode: snapshot` or unset)

1. `runSnapshotBackup`
   - `os.RemoveAll(<dest>/.unfinished)`, then recreate it. A previous partial run is
     **discarded**, not resumed.
   - `getLatestSnapshot` → newest snapshot dir becomes `--link-dest`.
   - `runRsync` into `.unfinished`. Stdout goes to `.unfinished/rsync.log`; stderr goes
     to both the console and the log.
   - `os.Rename(.unfinished, <dest>/<prefix>_<2006-01-02_15-04-05>)`. The rename is the
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

A snapshot is a directory directly under `destination` whose name is exactly
`<snapshot_prefix>_<2006-01-02_15-04-05>` (local time). `snapshotDirName` writes that
format and `parseSnapshotName` reads it. `parseSnapshotName` also accepts the legacy
`15:04:05` layout, because names with `:` break on SMB, exFAT and Windows. Without it,
pre-upgrade snapshots would be orphaned. Snapshots are ordered by the **timestamp in the name**,
and purge buckets use that same time. Directory mtimes are never consulted. Invariants:

1. **Anything else in `destination` is ignored.** That includes `lost+found`, other
   prefixes, and unrelated data. It's logged as "ignoring directory that is not a
   snapshot", never used as `--link-dest`, and **never purged**.
2. **Don't go back to mtime ordering.** With a trailing-slash source (`/data/`),
   `rsync -a` copies the source directory's mtime onto the snapshot root. That made new
   snapshots look old and broke `--link-dest` selection and retention.
3. **Changing `snapshot_prefix` orphans existing snapshots.** They stop being linked
   against or purged, and the next run is a full copy. This is deliberate: an
   unrecognized directory is left alone rather than deleted. Rename old snapshots by
   hand to migrate them.
4. The name format lives in `snapshotSep` and `snapshotTimeLayout` (plus
   `legacySnapshotTimeLayout`, which is parse-only). Never write `:` into a name again.
   Never build or match snapshot names by hand; tests use `snapshotDirName` and
   `legacyDirName`. Old and new names must be ordered by parsed time, never by string:
   within one hour `12-30-00` sorts before `12:00:00`
   (`TestGetSnapshots_OrdersMixedFormatsByTime`).
   `TestRunSnapshotBackup_CommitsSnapshot` checks that what's written can be parsed
   back.

## Configuration

| Key | Notes |
|---|---|
| `mode` | `snapshot` (default when empty) or `simple`; anything else is an error |
| `destination` | Required. Root for snapshots (snapshot mode) or the sync target (simple mode). In snapshot mode it **must already exist**; simple mode creates it |
| `snapshot_prefix` | Identifies which directories are snapshots; see *Snapshot discovery* |
| `source` | Required, non-empty, no blank entries, and every path must exist. Passed to rsync as given (trailing slashes matter to rsync). `destination` must not be inside any source |
| `exclude` | Each entry becomes `--exclude=<entry>` |
| `keep.daily/weekly/monthly` | Snapshot mode: non-negative, at least one > 0. All zero would purge every snapshot, including the new one |
| `rsync_extra_flags` | Whitespace-split string |
| `ignore_vanished_files_error` | Treat rsync exit 24 as success |

### Validation (`Config.validate`)

`Run` validates before anything touches disk, and `errors.Join` reports every
problem at once. Why the less obvious rules exist:

- **Snapshot destinations are never created.** When a backup drive isn't mounted, its
  mount point is empty or missing on the root filesystem, and a backup there can fill
  the disk. Simple mode keeps `MkdirAll`.
- **Destination inside a source** would copy every previous backup into the next one.
  This is rejected even when an `exclude` would cover it. `isWithin` and `resolvePath`
  make both paths absolute and resolve symlinks, including the nearest existing parent
  of a destination that doesn't exist yet, so `..`, relative paths and symlinks can't
  hide the overlap. Compare with `filepath.Rel`, never a string prefix: `/a/src-backups`
  is not inside `/a/src`.
- A source inside the destination is allowed.
- Relative paths are accepted. Under cron they resolve against a different working
  directory, so prefer absolute paths in real configs.

## Known issues and hazards

Fix these deliberately, with tests. Don't "tidy" around them.

1. **The legacy `:` layout can only be retired** once no destination still holds
   snapshots that use it. Until then, removing it orphans them.

## Testing conventions

- **The rsync mock** uses the stdlib helper-process pattern. `App.execCmd` is
  `exec.Command` in prod. Tests inject `newExecRecorder(exitCode).command`, which
  records each argv in `rec.calls` and re-executes the test binary into
  `TestHelperProcess`. That process exits with `GOBACK_HELPER_EXIT` and writes
  nothing, so the destination stays empty apart from what goback itself creates.
  Use `rec.onlyCall(t)` to assert on the single expected invocation.
- `TestRunRsyncArgs` pins the **exact** argv order: base flags, `--link-dest`,
  excludes, extra flags, `--dry-run`, sources, dest. Any change to `runRsync` argument
  building must update that table deliberately.
- **Gaps**: the purge-error path (logged, not returned) and the `rsync.log` Close-error
  path are untested. Inducing either needs a filesystem seam that doesn't exist.
- **Purge and discovery tests** name directories with
  `snapshotDirName("host", day(...))`. Use **fixed dates** (`day()` returns noon local
  time), never offsets from `time.Now()`, which make week and month bucketing depend on
  when the test runs. Purge itself never reads the clock. Give fixture directories
  mtimes that **contradict** their names (`mkdirWithMtime`), and add non-snapshot
  decoys. That's what proves ordering and filtering come from the name.
- Use `t.TempDir()` for every temp path. Helpers: `writeConfig` for config files,
  `newSnapshotApp(dest, rec)` for snapshot-mode tests, `dirNames` for listing.
  Source paths can be literals (`/src`), because the fake rsync never reads them.
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
