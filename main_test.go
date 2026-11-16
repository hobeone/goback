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

// mkdirWithMtime creates dir and sets its mtime.
func mkdirWithMtime(t *testing.T, dir string, mtime time.Time) {
	t.Helper()
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("Failed to create dir: %v", err)
	}
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatalf("Failed to set mod time: %v", err)
	}
}

func TestPurgeBackups(t *testing.T) {
	tmpDir := t.TempDir()

	config := &Config{
		Destination:    tmpDir,
		SnapshotPrefix: "host",
		Keep: Keep{
			Daily:   2,
			Weekly:  2,
			Monthly: 1,
		},
	}

	// Fixed dates, newest first, so the outcome cannot depend on when the
	// test runs. ISO weeks in 2025: Jun 16-22 = W25, Jun 9-15 = W24,
	// Jun 2-8 = W23, May 26-Jun 1 = W22.
	dates := []struct {
		t    time.Time
		keep bool
	}{
		{day(2025, time.June, 18), true},   // W25: daily
		{day(2025, time.June, 17), true},   // W25: daily
		{day(2025, time.June, 16), false},  // W25: bucket already claimed by a daily
		{day(2025, time.June, 11), true},   // W24: weekly
		{day(2025, time.June, 10), false},  // W24: bucket taken
		{day(2025, time.June, 4), true},    // W23: weekly
		{day(2025, time.June, 3), false},   // W23: bucket taken
		{day(2025, time.May, 28), true},    // weekly slots full; first unclaimed month -> monthly
		{day(2025, time.May, 27), false},   // May already claimed
		{day(2025, time.April, 15), false}, // monthly slots full
	}

	expected := make(map[string]bool)
	for i, d := range dates {
		name := snapshotDirName("host", d.t)
		// Directory mtimes run in the opposite order to the names, as can
		// happen when rsync copies a source dir's mtime onto the snapshot.
		// Retention must follow the names.
		mkdirWithMtime(t, filepath.Join(tmpDir, name), day(2030, time.January, 1).AddDate(0, 0, i))
		if d.keep {
			expected[name] = true
		}
	}

	// Directories that are not snapshots of this prefix must never be purged.
	for _, decoy := range []string{
		"lost+found",
		snapshotDirName("other", day(2025, time.April, 1)),
		"host_not-a-timestamp",
		day(2025, time.April, 1).Format(snapshotTimeLayout), // no prefix
	} {
		mkdirWithMtime(t, filepath.Join(tmpDir, decoy), day(2020, time.January, 1))
		expected[decoy] = true
	}

	app := &App{config: config, dryRun: false}
	if err := app.purgeBackups(); err != nil {
		t.Fatalf("purgeBackups failed: %v", err)
	}

	found := make(map[string]bool)
	for _, name := range dirNames(t, tmpDir) {
		found[name] = true
	}

	for name := range expected {
		if !found[name] {
			t.Errorf("Expected %s to be kept, but it was deleted", name)
		}
	}
	for name := range found {
		if !expected[name] {
			t.Errorf("Expected %s to be deleted, but it was kept", name)
		}
	}
}

func TestParseSnapshotName(t *testing.T) {
	tests := []struct {
		name, prefix string
		want         time.Time
		ok           bool
	}{
		// Current format.
		{"host_2025-06-18_13-14-15", "host", time.Date(2025, 6, 18, 13, 14, 15, 0, time.Local), true},
		{"_2025-06-18_13-14-15", "", time.Date(2025, 6, 18, 13, 14, 15, 0, time.Local), true},
		{"my_host_2025-06-18_13-14-15", "my_host", time.Date(2025, 6, 18, 13, 14, 15, 0, time.Local), true},
		{"my_host_2025-06-18_13-14-15", "my", time.Time{}, false}, // prefix must match exactly
		{"other_2025-06-18_13-14-15", "host", time.Time{}, false},
		{"host_2025-06-18", "host", time.Time{}, false},
		{"host_2025-06-18_13-14-15.bak", "host", time.Time{}, false},
		{"host-2025-06-18_13-14-15", "host", time.Time{}, false},
		{"lost+found", "host", time.Time{}, false},
		// Legacy colon format, still recognised after upgrade.
		{"host_2025-06-18_13:14:15", "host", time.Date(2025, 6, 18, 13, 14, 15, 0, time.Local), true},
		{"my_host_2025-06-18_13:14:15", "my", time.Time{}, false},
		{"host_2025-06-18_13:14:15.bak", "host", time.Time{}, false},
		// A mix of the two is neither.
		{"host_2025-06-18_13-14:15", "host", time.Time{}, false},
	}
	for _, tt := range tests {
		got, ok := parseSnapshotName(tt.name, tt.prefix)
		if ok != tt.ok || !got.Equal(tt.want) {
			t.Errorf("parseSnapshotName(%q, %q) = %v, %v; want %v, %v", tt.name, tt.prefix, got, ok, tt.want, tt.ok)
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
					Source:                   []string{"/src"},
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

// newSnapshotApp returns a valid snapshot-mode App backing up to dest with
// prefix "host" and keep.daily 1, with rsync replaced by rec. The source is
// an empty temp dir: it must exist to pass validation, but the fake rsync
// never reads it.
func newSnapshotApp(t *testing.T, dest string, rec *execRecorder) *App {
	t.Helper()
	return &App{
		config: &Config{
			Destination:    dest,
			SnapshotPrefix: "host",
			Source:         []string{t.TempDir()},
			Keep:           Keep{Daily: 1},
		},
		execCmd: rec.command,
	}
}

func TestRunSnapshotBackup_CommitsSnapshot(t *testing.T) {
	dest := t.TempDir()
	rec := newExecRecorder(0)
	app := newSnapshotApp(t, dest, rec)

	if err := app.runSnapshotBackup(); err != nil {
		t.Fatalf("runSnapshotBackup failed: %v", err)
	}

	// First snapshot: rsync writes into .unfinished with no --link-dest.
	argv := rec.onlyCall(t)
	if got, want := argv[len(argv)-1], filepath.Join(dest, ".unfinished"); got != want {
		t.Errorf("rsync dest = %q, want %q", got, want)
	}
	if slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "--link-dest") }) {
		t.Errorf("unexpected --link-dest on first snapshot: %q", argv)
	}

	// .unfinished was renamed into a single prefixed snapshot holding rsync.log.
	names := dirNames(t, dest)
	if len(names) != 1 {
		t.Fatalf("Expected one snapshot, got %q", names)
	}
	if strings.Contains(names[0], ":") {
		t.Errorf("snapshot name %q contains ':', which SMB/exFAT/Windows reject", names[0])
	}
	// The name goback writes must be one it later recognises as a snapshot.
	if _, ok := parseSnapshotName(names[0], "host"); !ok {
		t.Errorf("created snapshot %q is not recognised by parseSnapshotName", names[0])
	}
	if _, err := os.Stat(filepath.Join(dest, names[0], "rsync.log")); err != nil {
		t.Errorf("rsync.log missing from snapshot: %v", err)
	}
}

func TestRunSnapshotBackup_LinksAgainstLatest(t *testing.T) {
	dest := t.TempDir()
	// mtimes contradict the names (as after rsync copies a source dir's
	// mtime), and a non-snapshot directory is the most recently modified.
	// The newest snapshot by name must still win.
	older := snapshotDirName("host", day(2025, time.June, 17))
	newer := snapshotDirName("host", day(2025, time.June, 18))
	mkdirWithMtime(t, filepath.Join(dest, older), day(2025, time.June, 30))
	mkdirWithMtime(t, filepath.Join(dest, newer), day(2020, time.January, 1))
	mkdirWithMtime(t, filepath.Join(dest, "lost+found"), day(2026, time.January, 1))

	rec := newExecRecorder(0)
	app := newSnapshotApp(t, dest, rec)
	if err := app.runSnapshotBackup(); err != nil {
		t.Fatalf("runSnapshotBackup failed: %v", err)
	}

	want := "--link-dest=" + filepath.Join(dest, newer)
	if argv := rec.onlyCall(t); !slices.Contains(argv, want) {
		t.Errorf("argv %q does not contain %q", argv, want)
	}
}

func TestRunSnapshotBackup_FailureLeavesNoSnapshot(t *testing.T) {
	dest := t.TempDir()
	app := newSnapshotApp(t, dest, newExecRecorder(23))
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
	app := newSnapshotApp(t, dest, rec)
	app.dryRun = true
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
			Source:      []string{t.TempDir()},
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
	if err := app.Run(); err == nil || !strings.Contains(err.Error(), "invalid backup mode") {
		t.Fatalf("Run should reject an unknown mode, got %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("rsync ran despite invalid mode: %q", rec.calls)
	}
}

// Validation relies on any single non-zero count keeping the newest
// snapshot; pin that for the weekly-only and monthly-only cases.
func TestPurgeBackups_SingleSlotKeepsNewest(t *testing.T) {
	for _, keep := range []Keep{{Weekly: 1}, {Monthly: 1}} {
		t.Run(fmt.Sprintf("%+v", keep), func(t *testing.T) {
			dest := t.TempDir()
			newest := snapshotDirName("host", day(2025, time.June, 18))
			mkdirWithMtime(t, filepath.Join(dest, newest), day(2025, time.June, 18))
			mkdirWithMtime(t, filepath.Join(dest, snapshotDirName("host", day(2025, time.June, 17))), day(2025, time.June, 17))

			app := &App{config: &Config{Destination: dest, SnapshotPrefix: "host", Keep: keep}}
			if err := app.purgeBackups(); err != nil {
				t.Fatalf("purgeBackups failed: %v", err)
			}
			if names := dirNames(t, dest); !slices.Equal(names, []string{newest}) {
				t.Errorf("Expected only %s to survive, got %q", newest, names)
			}
		})
	}
}

// legacyDirName returns the pre-upgrade name, with ':' in the time.
func legacyDirName(prefix string, t time.Time) string {
	return prefix + snapshotSep + t.Format(legacySnapshotTimeLayout)
}

// After upgrading, the newest snapshot is still a legacy-named one, and the
// first new-format run must hard-link against it.
func TestRunSnapshotBackup_LinksAgainstLegacySnapshot(t *testing.T) {
	dest := t.TempDir()
	newest := legacyDirName("host", day(2025, time.June, 18))
	mkdirWithMtime(t, filepath.Join(dest, legacyDirName("host", day(2025, time.June, 17))), day(2025, time.June, 17))
	mkdirWithMtime(t, filepath.Join(dest, newest), day(2025, time.June, 18))

	rec := newExecRecorder(0)
	if err := newSnapshotApp(t, dest, rec).runSnapshotBackup(); err != nil {
		t.Fatalf("runSnapshotBackup failed: %v", err)
	}
	want := "--link-dest=" + filepath.Join(dest, newest)
	if argv := rec.onlyCall(t); !slices.Contains(argv, want) {
		t.Errorf("argv %q does not contain %q", argv, want)
	}
}

// Legacy and current names must order by their timestamps, not by name.
// Within the same hour "12-30-00" sorts before "12:00:00" as a string
// ('-' < ':'), so a name-based sort would put the 12:30 snapshot first.
func TestGetSnapshots_OrdersMixedFormatsByTime(t *testing.T) {
	dest := t.TempDir()
	noon := day(2025, time.June, 17)
	want := []string{
		snapshotDirName("host", day(2025, time.June, 16)),
		legacyDirName("host", noon),
		snapshotDirName("host", noon.Add(30*time.Minute)),
		legacyDirName("host", noon.Add(45*time.Minute)),
	}
	for _, name := range want {
		mkdirWithMtime(t, filepath.Join(dest, name), day(2020, time.January, 1))
	}

	snapshots, err := getSnapshots(dest, "host")
	if err != nil {
		t.Fatalf("getSnapshots failed: %v", err)
	}
	var got []string
	for _, s := range snapshots {
		got = append(got, s.name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("order mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		// edit adjusts a valid snapshot-mode config whose destination and
		// single source are separate existing temp dirs under root.
		edit func(t *testing.T, root string, c *Config)
		// wantErrs are substrings the error must contain; none means valid.
		wantErrs []string
	}{
		{name: "valid snapshot config", edit: func(*testing.T, string, *Config) {}},
		{
			name:     "empty destination",
			edit:     func(_ *testing.T, _ string, c *Config) { c.Destination = "" },
			wantErrs: []string{"destination must be set"},
		},
		{
			name:     "snapshot destination missing",
			edit:     func(_ *testing.T, root string, c *Config) { c.Destination = filepath.Join(root, "unmounted") },
			wantErrs: []string{"must already exist"},
		},
		{
			name: "snapshot destination is a file",
			edit: func(t *testing.T, root string, c *Config) {
				c.Destination = filepath.Join(root, "file")
				if err := os.WriteFile(c.Destination, nil, 0644); err != nil {
					t.Fatal(err)
				}
			},
			wantErrs: []string{"is not a directory"},
		},
		{
			name: "simple mode may create its destination",
			edit: func(_ *testing.T, root string, c *Config) {
				c.Mode = "simple"
				c.Destination = filepath.Join(root, "new", "dir")
			},
		},
		{
			name:     "no sources",
			edit:     func(_ *testing.T, _ string, c *Config) { c.Source = nil },
			wantErrs: []string{"at least one path"},
		},
		{
			name:     "blank source",
			edit:     func(_ *testing.T, _ string, c *Config) { c.Source = append(c.Source, "  ") },
			wantErrs: []string{"source[1] is empty"},
		},
		{
			name:     "missing source",
			edit:     func(_ *testing.T, root string, c *Config) { c.Source = append(c.Source, filepath.Join(root, "gone")) },
			wantErrs: []string{"gone"},
		},
		{
			name:     "destination equals source",
			edit:     func(_ *testing.T, _ string, c *Config) { c.Destination = c.Source[0] },
			wantErrs: []string{"inside source"},
		},
		{
			name: "destination under source",
			edit: func(t *testing.T, _ string, c *Config) {
				c.Destination = filepath.Join(c.Source[0], "backups")
				mustMkdir(t, c.Destination)
			},
			wantErrs: []string{"inside source"},
		},
		{
			name: "destination under source via ..",
			edit: func(t *testing.T, _ string, c *Config) {
				mustMkdir(t, filepath.Join(c.Source[0], "backups"))
				c.Destination = c.Source[0] + "/../" + filepath.Base(c.Source[0]) + "/backups"
			},
			wantErrs: []string{"inside source"},
		},
		{
			name: "destination under symlinked source",
			edit: func(t *testing.T, root string, c *Config) {
				link := filepath.Join(root, "link")
				if err := os.Symlink(c.Source[0], link); err != nil {
					t.Fatal(err)
				}
				c.Destination = filepath.Join(c.Source[0], "backups")
				mustMkdir(t, c.Destination)
				c.Source = []string{link + "/"}
			},
			wantErrs: []string{"inside source"},
		},
		{
			name: "simple mode destination not yet created under source",
			edit: func(_ *testing.T, _ string, c *Config) {
				c.Mode = "simple"
				c.Destination = filepath.Join(c.Source[0], "a", "b")
			},
			wantErrs: []string{"inside source"},
		},
		{
			// The destination does not exist and is reached through a
			// symlink, so only resolving its nearest existing parent shows
			// that it lands inside the source.
			name: "uncreated destination under a symlink to the source",
			edit: func(t *testing.T, root string, c *Config) {
				link := filepath.Join(root, "link")
				if err := os.Symlink(c.Source[0], link); err != nil {
					t.Fatal(err)
				}
				c.Mode = "simple"
				c.Destination = filepath.Join(link, "a", "b")
			},
			wantErrs: []string{"inside source"},
		},
		{
			// "..foo" is a normal name, not a parent reference; a naive
			// strings.HasPrefix(rel, "..") check would miss this overlap.
			name: "destination in a ..-prefixed directory under source",
			edit: func(t *testing.T, _ string, c *Config) {
				c.Destination = filepath.Join(c.Source[0], "..backups")
				mustMkdir(t, c.Destination)
			},
			wantErrs: []string{"inside source"},
		},
		{
			name: "sibling sharing a name prefix is not inside",
			edit: func(t *testing.T, _ string, c *Config) {
				c.Destination = c.Source[0] + "-backups"
				mustMkdir(t, c.Destination)
			},
		},
		{
			name: "source inside destination is allowed",
			edit: func(t *testing.T, _ string, c *Config) {
				src := filepath.Join(c.Destination, "data")
				mustMkdir(t, src)
				c.Source = []string{src}
			},
		},
		{
			name:     "missing keep block",
			edit:     func(_ *testing.T, _ string, c *Config) { c.Keep = Keep{} },
			wantErrs: []string{"at least one of daily, weekly or monthly"},
		},
		{
			name:     "negative keep count",
			edit:     func(_ *testing.T, _ string, c *Config) { c.Keep = Keep{Daily: 7, Monthly: -2} },
			wantErrs: []string{"must not be negative"},
		},
		{name: "weekly-only keep", edit: func(_ *testing.T, _ string, c *Config) { c.Keep = Keep{Weekly: 1} }},
		{name: "monthly-only keep", edit: func(_ *testing.T, _ string, c *Config) { c.Keep = Keep{Monthly: 1} }},
		{
			name: "simple mode ignores keep",
			edit: func(_ *testing.T, _ string, c *Config) {
				c.Mode = "simple"
				c.Keep = Keep{Daily: -1}
			},
		},
		{
			name:     "unknown mode",
			edit:     func(_ *testing.T, _ string, c *Config) { c.Mode = "bogus" },
			wantErrs: []string{`invalid backup mode: "bogus"`},
		},
		{
			name: "every problem is reported at once",
			edit: func(_ *testing.T, _ string, c *Config) {
				c.Destination = ""
				c.Source = nil
				c.Keep = Keep{}
			},
			wantErrs: []string{"destination must be set", "at least one path", "keep"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			c := &Config{
				Destination: filepath.Join(root, "dest"),
				Source:      []string{filepath.Join(root, "src")},
				Keep:        Keep{Daily: 1},
			}
			mustMkdir(t, c.Destination)
			mustMkdir(t, c.Source[0])
			tt.edit(t, root, c)

			err := c.validate()
			if len(tt.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() = nil, want errors containing %q", tt.wantErrs)
			}
			for _, want := range tt.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("validate() = %q, missing %q", err, want)
				}
			}
		})
	}
}

func TestRun_InvalidConfigTouchesNothing(t *testing.T) {
	root := t.TempDir()
	rec := newExecRecorder(0)
	app := newSnapshotApp(t, filepath.Join(root, "unmounted"), rec)
	if err := app.Run(); err == nil {
		t.Fatal("Run should fail when the destination is missing")
	}
	if len(rec.calls) != 0 {
		t.Errorf("rsync ran despite invalid config: %q", rec.calls)
	}
	if names := dirNames(t, root); len(names) != 0 {
		t.Errorf("Run created %q despite invalid config", names)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
}
