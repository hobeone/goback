package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func TestRunSnapshotBackupIgnoreVanishedFilesError(t *testing.T) {
	config := &Config{
		Destination:              t.TempDir(),
		SnapshotPrefix:           "test",
		Source:                   []string{newSourceDir(t)},
		IgnoreVanishedFilesError: true,
	}

	app := &App{config: config, dryRun: false, execCmd: mockExecCommand}
	if err := app.runSnapshotBackup(); err != nil {
		t.Fatalf("runSnapshotBackup failed: %v", err)
	}
}

func TestRunSnapshotBackupIgnoreVanishedFilesError_False(t *testing.T) {
	config := &Config{
		Destination:              t.TempDir(),
		SnapshotPrefix:           "test",
		Source:                   []string{newSourceDir(t)},
		IgnoreVanishedFilesError: false,
	}

	app := &App{config: config, dryRun: false, execCmd: mockExecCommand}
	if err := app.runSnapshotBackup(); err == nil {
		t.Fatal("runSnapshotBackup should have failed but didn't")
	}
}

func TestRunSimpleBackup(t *testing.T) {
	config := &Config{
		Mode:                     "simple",
		Destination:              t.TempDir(),
		Source:                   []string{t.TempDir()},
		IgnoreVanishedFilesError: true,
	}

	app := &App{config: config, dryRun: false, execCmd: mockExecCommand}
	if err := app.runSimpleBackup(); err != nil {
		t.Fatalf("runSimpleBackup failed: %v", err)
	}
}

// mockExecCommand re-executes the test binary as TestHelperProcess, which
// stands in for the real command.
func mockExecCommand(command string, args ...string) *exec.Cmd {
	cs := []string{"-test.run=TestHelperProcess", "--", command}
	cs = append(cs, args...)
	cmd := exec.Command(os.Args[0], cs...)
	cmd.Env = []string{"GO_WANT_HELPER_PROCESS=1"}
	return cmd
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	// Skip past the "--" separator to find the command we're standing in for.
	args := os.Args
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "No command\n")
		os.Exit(2)
	}

	if args[0] == "rsync" {
		// Simulate rsync exiting with code 24 (vanished files).
		os.Exit(24)
	}
}
