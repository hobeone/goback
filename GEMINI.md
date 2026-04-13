# GEMINI Context: GoBack

GoBack is a Go-based, file-driven backup utility that utilizes `rsync` and hard links to create space-efficient, versioned snapshots with an automated retention policy.

## Project Overview

- **Purpose**: Automates incremental backups using `rsync --link-dest` to minimize disk usage while maintaining a history of snapshots.
- **Core Technologies**: 
    - **Language**: Go (v1.24+)
    - **External Dependency**: `rsync` (must be in system PATH)
    - **Configuration**: YAML
    - **Logging**: `zerolog` for structured console and file logging.
- **Architecture**: A CLI tool that performs two main phases:
    1. **Backup Phase**: Synchronizes source directories to a new timestamped snapshot in the destination, linking unchanged files to the previous snapshot.
    2. **Purge Phase**: Evaluates existing snapshots against a GFS (Grandfather-Father-Son) retention policy (daily, weekly, monthly) and deletes expired backups.

## Building and Running

### Development Commands
- **Run**: `go run main.go [-config config.yaml] [-dry-run]`
- **Build**: `go build -o goback main.go`
- **Test**: `go test ./...` (Includes mocked `rsync` execution to test error handling and retention logic).

### Execution Modes
- **Snapshot (Default)**: Creates versioned snapshots and applies retention policies.
- **Simple**: Performs a standard `rsync` sync without versioning or purging.

## Configuration (`config.yaml`)
Key parameters include:
- `destination`: Root directory for all snapshots.
- `source`: List of directories/files to back up.
- `keep`: Retention counts for `daily`, `weekly`, and `monthly` snapshots.
- `rsync_extra_flags`: Custom flags passed directly to the `rsync` command.
- `ignore_vanished_files_error`: If true, treats `rsync` exit code 24 as a non-fatal warning.

## Development Conventions

- **Error Handling**: Uses standard Go error wrapping and `zerolog` for fatal error reporting.
- **Testing Strategy**: 
    - Logic for purging is tested by manipulating file modification times in temporary directories.
    - Shell command execution is tested by mocking `exec.Command` and using a helper process to simulate exit codes.
- **Style**: Standard Go idioms; `main.go` contains the bulk of the logic.
- **Logging**: Console output is formatted for readability, while `rsync` output for snapshots is redirected to `rsync.log` within the snapshot directory itself.
