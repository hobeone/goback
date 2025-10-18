package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var dryRun = flag.Bool("dry-run", false, "print actions without executing them")

type Config struct {
	Destination      string   `yaml:"destination"`
	SnapshotPrefix   string   `yaml:"snapshot_prefix"`
	Source           []string `yaml:"source"`
	Exclude          []string `yaml:"exclude"`
	Keep             Keep     `yaml:"keep"`
	RsyncExtraFlags  string   `yaml:"rsync_extra_flags"`
}

type Keep struct {
	Daily   int `yaml:"daily"`
	Weekly  int `yaml:"weekly"`
	Monthly int `yaml:"monthly"`
}

func main() {
	flag.Parse()

	config, err := readConfig("config.yaml")
	if err != nil {
		log.Fatalf("error reading config: %v", err)
	}

	if err := runBackup(config, *dryRun); err != nil {
		log.Fatalf("backup failed: %v", err)
	}

	if err := purgeBackups(config, *dryRun); err != nil {
		log.Fatalf("purging old backups failed: %v", err)
	}
}

func readConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}

func runBackup(config *Config, dryRun bool) error {
	log.Printf("Snapshot: %v to %s", config.Source, config.Destination)

	unfinishedDir := filepath.Join(config.Destination, ".unfinished")
	snapshotName := fmt.Sprintf("%s_%s", config.SnapshotPrefix, time.Now().Format("2006-01-02_15:04:05"))
	finalDest := filepath.Join(config.Destination, snapshotName)

	latestSnapshot, err := getLatestSnapshot(config.Destination)
	if err != nil {
		return fmt.Errorf("failed to get latest snapshot: %w", err)
	}

	args := []string{"-a", "-v", "-h", "--delete", "--stats", "--inplace"}
	if latestSnapshot != "" {
		args = append(args, "--link-dest="+filepath.Join(config.Destination, latestSnapshot))
	}
	for _, ex := range config.Exclude {
		args = append(args, "--exclude="+ex)
	}
	if config.RsyncExtraFlags != "" {
		args = append(args, strings.Split(config.RsyncExtraFlags, " ")...)
	}

	if dryRun {
		hasDryRun := false
		for _, arg := range args {
			if arg == "--dry-run" || arg == "-n" {
				hasDryRun = true
				break
			}
		}
		if !hasDryRun {
			args = append(args, "--dry-run")
		}
	}

	args = append(args, config.Source...)
	args = append(args, unfinishedDir)

	if dryRun {
		log.Println("[Dry Run] Not creating/removing any directories.")

		cmd := exec.Command("rsync", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		log.Printf("Running command: rsync %s", strings.Join(args, " "))
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("rsync command failed during dry run: %w", err)
		}

		log.Printf("[Dry Run] Would rename %s to %s", unfinishedDir, finalDest)
		log.Println("Backup finished successfully (dry run)")
		return nil
	}

	if err := os.RemoveAll(unfinishedDir); err != nil {
		return fmt.Errorf("failed to remove unfinished directory: %w", err)
	}
	if err := os.MkdirAll(unfinishedDir, 0755); err != nil {
		return fmt.Errorf("failed to create unfinished directory: %w", err)
	}

	cmd := exec.Command("rsync", args...)
	logFile, err := os.Create(filepath.Join(unfinishedDir, "rsync.log"))
	if err != nil {
		return fmt.Errorf("failed to create rsync log file: %w", err)
	}
	defer logFile.Close()

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	log.Printf("Running command: rsync %s", strings.Join(args, " "))

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync command failed: %w", err)
	}

	if err := os.Rename(unfinishedDir, finalDest); err != nil {
		return fmt.Errorf("failed to rename unfinished directory: %w", err)
	}

	log.Println("Backup finished successfully")
	return nil
}

func getSnapshots(dest string) ([]os.FileInfo, error) {
	entries, err := os.ReadDir(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var snapshots []os.FileInfo
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			snapshots = append(snapshots, info)
		}
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].ModTime().Before(snapshots[j].ModTime())
	})

	return snapshots, nil
}

func getLatestSnapshot(dest string) (string, error) {
	snapshots, err := getSnapshots(dest)
	if err != nil || len(snapshots) == 0 {
		return "", err
	}
	return snapshots[len(snapshots)-1].Name(), nil
}

func ageInDays(fi os.FileInfo) int {
	return int(time.Since(fi.ModTime()).Hours() / 24)
}

func purgeBackups(config *Config, dryRun bool) error {
	snapshots, err := getSnapshots(config.Destination)
	if err != nil {
		return err
	}

	var to_keep []os.FileInfo
	if len(snapshots) == 0 {
		return nil
	}

	to_keep = append(to_keep, snapshots[len(snapshots)-1])
	last_kept := snapshots[len(snapshots)-1]

	for i := len(snapshots) - 2; i >= 0; i-- {
		s := snapshots[i]
		age := ageInDays(s)

		interval := 0
		if age <= config.Keep.Daily {
			interval = 0 // keep all
		} else if age <= config.Keep.Daily+config.Keep.Weekly*7 {
			interval = 7
		} else {
			interval = 30
		}

		age_diff := ageInDays(s) - ageInDays(last_kept)

		if interval == 0 || age_diff >= interval {
			to_keep = append(to_keep, s)
			last_kept = s
		}
	}

	keep_map := make(map[string]bool)
	for _, s := range to_keep {
		keep_map[s.Name()] = true
	}

	for _, s := range snapshots {
		if !keep_map[s.Name()] {
			if dryRun {
				log.Printf("[Dry Run] Would purge snapshot: %s", s.Name())
			} else {
				log.Printf("Purging snapshot: %s", s.Name())
				err := os.RemoveAll(filepath.Join(config.Destination, s.Name()))
				if err != nil {
					log.Printf("Failed to purge snapshot %s: %v", s.Name(), err)
				}
			}
		}
	}

	return nil
}
