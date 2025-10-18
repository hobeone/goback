# GoBack

GoBack is a simple, file-based backup tool written in Go. It uses `rsync` to create space-efficient, versioned snapshots of your files and directories.

## Features

-   Uses `rsync` with hard links to save disk space.
-   Configuration via a simple YAML file.
-   Flexible retention policy (daily, weekly, monthly).
-   Dry run mode to preview changes.
-   Detailed logging.

## Dependencies

-   **Go**: Version 1.18 or higher.
-   **rsync**: Must be installed and available in your system's PATH.

## Installation

1.  Clone this repository.
2.  Install the dependencies:
    ```bash
    go get
    ```

## Configuration

Configuration is managed through a `config.yaml` file. You can use the `-config` flag to specify a different path for this file.

Here is an example `config.yaml`:

```yaml
destination: /mnt/backups/my_server
snapshot_prefix: server
source:
  - /home/user/documents
  - /etc/nginx
exclude:
  - /home/user/documents/cache
  - "*.log"
keep:
  daily: 7
  weekly: 4
  monthly: 6
rsync_extra_flags: "--compress"
```

### Configuration Options

-   `destination`: The directory where snapshots will be stored.
-   `snapshot_prefix`: A prefix for the snapshot directory names (e.g., `server_2025-10-18_13:14:20`).
-   `source`: A list of files and directories to back up.
-   `exclude`: A list of patterns to exclude from the backup. These are passed to `rsync`'s `--exclude` flag.
-   `keep`: Specifies the number of snapshots to keep for each category.
    -   `daily`: Number of the most recent daily backups to keep.
    -   `weekly`: Number of the most recent weekly backups to keep (keeps the newest snapshot from each week).
    -   `monthly`: Number of the most recent monthly backups to keep (keeps the newest snapshot from each month).
-   `rsync_extra_flags`: A string of extra flags to pass to the `rsync` command (e.g., `"--compress --bwlimit=1000"`).

## Usage

To run the backup and purge process, execute the following command:

```bash
go run main.go
```

### Command-Line Flags

-   `-config <path>`: Specifies the path to the configuration file. Defaults to `config.yaml`.
    ```bash
    go run main.go -config /path/to/my_config.yaml
    ```
-   `-dry-run`: Runs the script in dry run mode. It will print the actions it would take without actually modifying any files. This includes running `rsync` with its own `--dry-run` flag to show you what files would be transferred.
    ```bash
    go run main.go -dry-run
    ```

## How It Works

### Backup Process

1.  The tool creates a temporary `.unfinished` directory in the destination.
2.  It finds the most recent existing snapshot.
3.  It runs `rsync` to copy the source files to the `.unfinished` directory. The `--link-dest` option is used to create hard links to files in the most recent snapshot, which means unchanged files are not copied again, saving space.
4.  If the `rsync` command is successful, the `.unfinished` directory is renamed to a new snapshot name, which includes the current date and time.

### Purging Process

The script purges old backups based on the `keep` configuration:

1.  It keeps the `keep.daily` most recent snapshots.
2.  It then keeps the `keep.weekly` most recent weekly snapshots. A weekly snapshot is the newest snapshot within a given calendar week.
3.  Finally, it keeps the `keep.monthly` most recent monthly snapshots. A monthly snapshot is the newest snapshot within a given calendar month.
4.  Any snapshot not selected to be kept is deleted.
