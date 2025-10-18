package main

import (
	"fmt"
	"os"
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
			Daily:   7,
			Weekly:  4,
			Monthly: 4,
		},
	}

	ages := []int{1, 2, 3, 4, 5, 6, 7, 8, 10, 15, 18, 22, 25, 29, 32, 40, 50, 70, 80, 130}
	now := time.Now()
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
	if err := purgeBackups(config); err != nil {
		t.Fatalf("purgeBackups failed: %v", err)
	}

	// Verify
	expected_to_keep := map[string]bool{
		"snapshot-1":   true,
		"snapshot-2":   true,
		"snapshot-3":   true,
		"snapshot-4":   true,
		"snapshot-5":   true,
		"snapshot-6":   true,
		"snapshot-7":   true,
		"snapshot-15":  true,
		"snapshot-22":  true,
		"snapshot-29":  true,
		"snapshot-70":  true,
		"snapshot-130": true,
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
