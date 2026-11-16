package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// day returns noon local time on the given date. Noon keeps ISOWeek and
// month bucketing stable regardless of time zone or DST shifts.
func day(year int, month time.Month, d int) time.Time {
	return time.Date(year, month, d, 12, 0, 0, 0, time.Local)
}

func TestPurgeBackups(t *testing.T) {
	tmpDir := t.TempDir()

	config := &Config{
		Destination: tmpDir,
		Keep: Keep{
			Daily:   2,
			Weekly:  2,
			Monthly: 1,
		},
	}

	// Fixed dates, so the outcome cannot depend on when the test runs.
	// ISO weeks in 2025: Jun 16-22 = W25, Jun 9-15 = W24, Jun 2-8 = W23,
	// May 26-Jun 1 = W22.
	snapshots := map[string]time.Time{
		"2025-06-18": day(2025, time.June, 18),  // W25: daily
		"2025-06-17": day(2025, time.June, 17),  // W25: daily
		"2025-06-16": day(2025, time.June, 16),  // W25: bucket already claimed by a daily -> purge
		"2025-06-11": day(2025, time.June, 11),  // W24: weekly
		"2025-06-10": day(2025, time.June, 10),  // W24: purge
		"2025-06-04": day(2025, time.June, 4),   // W23: weekly
		"2025-06-03": day(2025, time.June, 3),   // W23: purge
		"2025-05-28": day(2025, time.May, 28),   // weekly slots full; May is first unclaimed month -> monthly
		"2025-05-27": day(2025, time.May, 27),   // May already claimed -> purge
		"2025-04-15": day(2025, time.April, 15), // monthly slots full -> purge
	}
	for name, modTime := range snapshots {
		path := filepath.Join(tmpDir, name)
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatalf("Failed to create dir: %v", err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("Failed to set mod time: %v", err)
		}
	}

	app := &App{config: config, dryRun: false}
	if err := app.purgeBackups(); err != nil {
		t.Fatalf("purgeBackups failed: %v", err)
	}

	expectedToKeep := map[string]bool{
		"2025-06-18": true, // daily
		"2025-06-17": true, // daily
		"2025-06-11": true, // weekly
		"2025-06-04": true, // weekly
		"2025-05-28": true, // monthly
	}

	files, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read dir: %v", err)
	}

	found := make(map[string]bool)
	for _, f := range files {
		found[f.Name()] = true
	}

	if len(found) != len(expectedToKeep) {
		t.Errorf("Expected %d snapshots, but found %d", len(expectedToKeep), len(found))
	}
	for name := range expectedToKeep {
		if !found[name] {
			t.Errorf("Expected to find snapshot %s, but it was deleted", name)
		}
	}
	for name := range found {
		if !expectedToKeep[name] {
			t.Errorf("Expected snapshot %s to be deleted, but it was kept", name)
		}
	}
}

// writeConfig writes content to a config file in a per-test temp dir and
// returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}
	return path
}

func TestReadConfig(t *testing.T) {
	path := writeConfig(t, `
destination: /tmp/backup
snapshot_prefix: test
source:
  - /tmp/source1
exclude:
  - /tmp/source1/excluded
keep:
  daily: 1
  weekly: 1
  monthly: 1
rsync_extra_flags: "--verbose"
ignore_vanished_files_error: true
`)

	config, err := readConfig(path)
	if err != nil {
		t.Fatalf("readConfig failed: %v", err)
	}

	if config.Destination != "/tmp/backup" {
		t.Errorf("Expected destination '/tmp/backup', got '%s'", config.Destination)
	}
	if config.Mode != "" && config.Mode != "snapshot" {
		t.Errorf("Expected mode 'snapshot' or empty, got '%s'", config.Mode)
	}
	if config.SnapshotPrefix != "test" {
		t.Errorf("Expected snapshot_prefix 'test', got '%s'", config.SnapshotPrefix)
	}
	if len(config.Source) != 1 || config.Source[0] != "/tmp/source1" {
		t.Errorf("Expected source ['/tmp/source1'], got '%v'", config.Source)
	}
	if config.Keep.Daily != 1 {
		t.Errorf("Expected keep.daily 1, got %d", config.Keep.Daily)
	}
	if config.IgnoreVanishedFilesError != true {
		t.Errorf("Expected ignore_vanished_files_error true, got %v", config.IgnoreVanishedFilesError)
	}
}

func TestReadConfig_SimpleMode(t *testing.T) {
	path := writeConfig(t, `
mode: simple
destination: /tmp/backup
source:
  - /tmp/source1
`)

	config, err := readConfig(path)
	if err != nil {
		t.Fatalf("readConfig failed: %v", err)
	}

	if config.Mode != "simple" {
		t.Errorf("Expected mode 'simple', got '%s'", config.Mode)
	}
	if config.Destination != "/tmp/backup" {
		t.Errorf("Expected destination '/tmp/backup', got '%s'", config.Destination)
	}
}

func TestReadConfig_NotFound(t *testing.T) {
	_, err := readConfig("non-existent-file.yaml")
	if err == nil {
		t.Errorf("Expected an error when reading a non-existent file, but got nil")
	}
}

// newSourceDir creates a temp source directory containing one file.
func newSourceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "testfile"), []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	return dir
}

// execRecorder stands in for exec.Command. Each call is recorded, and the
// returned command re-executes the test binary as TestHelperProcess, which
// exits with exitCode.
type execRecorder struct {
	exitCode int
	calls    [][]string // command name followed by its args
}

func newExecRecorder(exitCode int) *execRecorder {
	return &execRecorder{exitCode: exitCode}
}

func (r *execRecorder) command(name string, args ...string) *exec.Cmd {
	r.calls = append(r.calls, append([]string{name}, args...))
	cs := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
	cmd := exec.Command(os.Args[0], cs...)
	cmd.Env = []string{
		"GO_WANT_HELPER_PROCESS=1",
		"GOBACK_HELPER_EXIT=" + strconv.Itoa(r.exitCode),
	}
	return cmd
}

// onlyCall asserts exactly one command was run and returns its argv.
func (r *execRecorder) onlyCall(t *testing.T) []string {
	t.Helper()
	if len(r.calls) != 1 {
		t.Fatalf("Expected exactly 1 command, got %d: %q", len(r.calls), r.calls)
	}
	return r.calls[0]
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	code, err := strconv.Atoi(os.Getenv("GOBACK_HELPER_EXIT"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad GOBACK_HELPER_EXIT: %v\n", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func TestRunRsyncArgs(t *testing.T) {
	base := []string{"rsync", "-a", "-v", "-h", "--delete", "--stats", "--inplace"}

	tests := []struct {
		name     string
		exclude  []string
		extra    string
		linkDest string
		dryRun   bool
		// want is the argv between the fixed base flags and the sources.
		want []string
	}{
		{
			name: "minimal",
			want: nil,
		},
		{
			name:     "link-dest, excludes and extra flags in order",
			exclude:  []string{"/src/cache", "*.log"},
			extra:    "  --compress   --bwlimit=1000 ",
			linkDest: "/backups/prev",
			want: []string{
				"--link-dest=/backups/prev",
				"--exclude=/src/cache", "--exclude=*.log",
				"--compress", "--bwlimit=1000",
			},
		},
		{
			name:   "dry run adds --dry-run",
			dryRun: true,
			want:   []string{"--dry-run"},
		},
		{
			name:   "dry run does not duplicate -n",
			extra:  "-n",
			dryRun: true,
			want:   []string{"-n"},
		},
		{
			name:   "dry run does not duplicate --dry-run",
			extra:  "--dry-run",
			dryRun: true,
			want:   []string{"--dry-run"},
		},
		{
			name:  "--dry-run in extra flags without -dry-run is passed through",
			extra: "--dry-run",
			want:  []string{"--dry-run"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			destDir := t.TempDir()
			rec := newExecRecorder(0)
			app := &App{
				config: &Config{
					Source:          []string{"/src/a", "/src/b/"},
					Exclude:         tt.exclude,
					RsyncExtraFlags: tt.extra,
				},
				dryRun:  tt.dryRun,
				execCmd: rec.command,
			}

			if err := app.runRsync(destDir, tt.linkDest); err != nil {
				t.Fatalf("runRsync failed: %v", err)
			}

			want := slices.Concat(base, tt.want, []string{"/src/a", "/src/b/", destDir})
			if got := rec.onlyCall(t); !slices.Equal(got, want) {
				t.Errorf("argv mismatch\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

func TestRunRsyncExitCodes(t *testing.T) {
	tests := []struct {
		name         string
		exitCode     int
		ignoreVanish bool
		wantErr      bool
	}{
		{"success", 0, false, false},
		{"vanished files ignored", 24, true, false},
		{"vanished files not ignored", 24, false, true},
		{"other failure is never ignored", 23, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &App{
				config: &Config{
					Source:                   []string{newSourceDir(t)},
					IgnoreVanishedFilesError: tt.ignoreVanish,
				},
				execCmd: newExecRecorder(tt.exitCode).command,
			}
			err := app.runRsync(t.TempDir(), "")
			if (err != nil) != tt.wantErr {
				t.Errorf("runRsync error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// dirNames returns the names of the entries in dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("Failed to read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestRunSnapshotBackup_CommitsSnapshot(t *testing.T) {
	dest := t.TempDir()
	rec := newExecRecorder(0)
	app := &App{
		config: &Config{
			Destination:    dest,
			SnapshotPrefix: "host",
			Source:         []string{newSourceDir(t)},
		},
		execCmd: rec.command,
	}

	if err := app.runSnapshotBackup(); err != nil {
		t.Fatalf("runSnapshotBackup failed: %v", err)
	}

	// First snapshot: rsync writes into .unfinished with no --link-dest.
	argv := rec.onlyCall(t)
	if got, want := argv[len(argv)-1], filepath.Join(dest, ".unfinished"); got != want {
		t.Errorf("rsync dest = %q, want %q", got, want)
	}
	for _, a := range argv {
		if strings.HasPrefix(a, "--link-dest") {
			t.Errorf("unexpected %q on first snapshot", a)
		}
	}

	// .unfinished was renamed into a single prefixed snapshot holding rsync.log.
	names := dirNames(t, dest)
	if len(names) != 1 || !strings.HasPrefix(names[0], "host_") {
		t.Fatalf("Expected one host_* snapshot, got %q", names)
	}
	if _, err := os.Stat(filepath.Join(dest, names[0], "rsync.log")); err != nil {
		t.Errorf("rsync.log missing from snapshot: %v", err)
	}
}

func TestRunSnapshotBackup_LinksAgainstLatest(t *testing.T) {
	dest := t.TempDir()
	for name, modTime := range map[string]time.Time{
		"host_2025-06-17_12:00:00": day(2025, time.June, 17),
		"host_2025-06-18_12:00:00": day(2025, time.June, 18),
	} {
		path := filepath.Join(dest, name)
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatalf("Failed to create dir: %v", err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("Failed to set mod time: %v", err)
		}
	}

	rec := newExecRecorder(0)
	app := &App{
		config: &Config{
			Destination:    dest,
			SnapshotPrefix: "host",
			Source:         []string{newSourceDir(t)},
		},
		execCmd: rec.command,
	}
	if err := app.runSnapshotBackup(); err != nil {
		t.Fatalf("runSnapshotBackup failed: %v", err)
	}

	want := "--link-dest=" + filepath.Join(dest, "host_2025-06-18_12:00:00")
	if argv := rec.onlyCall(t); !slices.Contains(argv, want) {
		t.Errorf("argv %q does not contain %q", argv, want)
	}
}

func TestRunSnapshotBackup_FailureLeavesNoSnapshot(t *testing.T) {
	dest := t.TempDir()
	app := &App{
		config: &Config{
			Destination:    dest,
			SnapshotPrefix: "host",
			Source:         []string{newSourceDir(t)},
		},
		execCmd: newExecRecorder(23).command,
	}
	if err := app.runSnapshotBackup(); err == nil {
		t.Fatal("runSnapshotBackup should have failed but didn't")
	}
	if names := dirNames(t, dest); !slices.Equal(names, []string{".unfinished"}) {
		t.Errorf("Expected only .unfinished after failure, got %q", names)
	}
}

func TestRunSnapshotBackup_DryRunTouchesNothing(t *testing.T) {
	dest := t.TempDir()
	rec := newExecRecorder(0)
	app := &App{
		config: &Config{
			Destination:    dest,
			SnapshotPrefix: "host",
			Source:         []string{newSourceDir(t)},
		},
		dryRun:  true,
		execCmd: rec.command,
	}
	if err := app.runSnapshotBackup(); err != nil {
		t.Fatalf("runSnapshotBackup failed: %v", err)
	}
	if !slices.Contains(rec.onlyCall(t), "--dry-run") {
		t.Errorf("rsync was not run with --dry-run: %q", rec.calls[0])
	}
	if names := dirNames(t, dest); len(names) != 0 {
		t.Errorf("dry run created entries in destination: %q", names)
	}
}

func TestRunSimpleBackup(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "not-yet-created")
	rec := newExecRecorder(0)
	app := &App{
		config: &Config{
			Mode:        "simple",
			Destination: dest,
			Source:      []string{newSourceDir(t)},
		},
		execCmd: rec.command,
	}
	if err := app.Run(); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	argv := rec.onlyCall(t)
	if got := argv[len(argv)-1]; got != dest {
		t.Errorf("rsync dest = %q, want %q", got, dest)
	}
	if info, err := os.Stat(dest); err != nil || !info.IsDir() {
		t.Errorf("destination was not created: %v", err)
	}
	// Simple mode logs to stdout, never into the destination.
	if names := dirNames(t, dest); len(names) != 0 {
		t.Errorf("simple mode wrote into destination: %q", names)
	}
}

func TestRun_InvalidMode(t *testing.T) {
	rec := newExecRecorder(0)
	app := &App{config: &Config{Mode: "bogus"}, execCmd: rec.command}
	if err := app.Run(); err == nil {
		t.Fatal("Run should reject an unknown mode")
	}
	if len(rec.calls) != 0 {
		t.Errorf("rsync ran despite invalid mode: %q", rec.calls)
	}
}
