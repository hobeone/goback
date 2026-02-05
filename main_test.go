package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPurgeBackups(t *testing.T) {
	// Setup
	tmpDir, err := os.MkdirTemp("", "goback-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	config := &Config{
		Destination: tmpDir,
		Keep: Keep{
			Daily:   2,
			Weekly:  2,
			Monthly: 1,
		},
	}

	// Create snapshots with specific dates
	// 2 daily, 2 weekly (from different weeks), 1 monthly, and some to be purged
	now := time.Now()
	ages := []int{
		1, 2, // Daily
		8, 9, // Week 2
		15, 16, // Week 3
		45, 46, // Month 2
		75, 76, // Month 3
	}
	for _, age := range ages {
		name := fmt.Sprintf("snapshot-%d", age)
		path := filepath.Join(tmpDir, name)
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatalf("Failed to create dir: %v", err)
		}
		modTime := now.AddDate(0, 0, -age)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("Failed to set mod time: %v", err)
		}
	}

	// Execute
	if err := purgeBackups(config, false); err != nil {
		t.Fatalf("purgeBackups failed: %v", err)
	}

	// Verify
	expected_to_keep := map[string]bool{
		"snapshot-1":  true, // daily
		"snapshot-2":  true, // daily
		"snapshot-8":  true, // weekly
		"snapshot-15": true, // weekly
		"snapshot-45": true, // monthly
	}

	files, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("Failed to read dir: %v", err)
	}

	found_map := make(map[string]bool)
	for _, f := range files {
		found_map[f.Name()] = true
	}

	if len(found_map) != len(expected_to_keep) {
		t.Errorf("Expected %d files, but found %d", len(expected_to_keep), len(found_map))
	}

	for name := range expected_to_keep {
		if !found_map[name] {
			t.Errorf("Expected to find snapshot %s, but it was deleted", name)
		}
	}
	for name := range found_map {
		if !expected_to_keep[name] {
			t.Errorf("Expected snapshot %s to be deleted, but it was kept", name)
		}
	}
}

func TestReadConfig(t *testing.T) {
	// Setup
	configFileContent := `
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
`
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(configFileContent); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Execute
	config, err := readConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("readConfig failed: %v", err)
	}

	// Verify
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
	// Setup
	configFileContent := `
mode: simple
destination: /tmp/backup
source:
  - /tmp/source1
`
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(configFileContent); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	tmpFile.Close()

	// Execute
	config, err := readConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("readConfig failed: %v", err)
	}

	// Verify
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

func TestRunSnapshotBackupIgnoreVanishedFilesError(t *testing.T) {
	// Setup
	tmpDir, err := os.MkdirTemp("", "goback-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sourceDir, err := os.MkdirTemp("", "goback-source")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(sourceDir)

	// Create a file in the source directory
	if err := os.WriteFile(filepath.Join(sourceDir, "testfile"), []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	config := &Config{
		Destination:              tmpDir,
		SnapshotPrefix:           "test",
		Source:                   []string{sourceDir},
		IgnoreVanishedFilesError: true,
	}

	execCommand = mockExecCommand
	defer func() { execCommand = exec.Command }()

	err = runSnapshotBackup(config, false)
	if err != nil {
		t.Fatalf("runSnapshotBackup failed: %v", err)
	}
}

func TestRunSnapshotBackupIgnoreVanishedFilesError_False(t *testing.T) {
	// Setup
	tmpDir, err := os.MkdirTemp("", "goback-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sourceDir, err := os.MkdirTemp("", "goback-source")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(sourceDir)

	// Create a file in the source directory
	if err := os.WriteFile(filepath.Join(sourceDir, "testfile"), []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	config := &Config{
		Destination:              tmpDir,
		SnapshotPrefix:           "test",
		Source:                   []string{sourceDir},
		IgnoreVanishedFilesError: false,
	}

	execCommand = mockExecCommand
	defer func() { execCommand = exec.Command }()

	err = runSnapshotBackup(config, false)
	if err == nil {
		t.Fatal("runSnapshotBackup should have failed but didn't")
	}
}

func TestRunSimpleBackup(t *testing.T) {
	// Setup
	tmpDir, err := os.MkdirTemp("", "goback-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sourceDir, err := os.MkdirTemp("", "goback-source")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(sourceDir)

	config := &Config{
		Mode:                     "simple",
		Destination:              tmpDir,
		Source:                   []string{sourceDir},
		IgnoreVanishedFilesError: true,
	}

	execCommand = mockExecCommand
	defer func() { execCommand = exec.Command }()

	err = runSimpleBackup(config, false)
	if err != nil {
		t.Fatalf("runSimpleBackup failed: %v", err)
	}
}
    
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
    	// Check the command line arguments to see which command we're supposed to be.
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
    
    	cmd, args := args[0], args[1:]
    	if cmd == "rsync" {
    		// Simulate rsync exiting with code 24
    		os.Exit(24)
    	}
    }
    