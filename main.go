package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

var dryRun = flag.Bool("dry-run", false, "print actions without executing them")
var configFile = flag.String("config", "config.yaml", "path to the configuration file")

type Config struct {
	Destination              string   `yaml:"destination"`
	SnapshotPrefix           string   `yaml:"snapshot_prefix"`
	Source                   []string `yaml:"source"`
	Exclude                  []string `yaml:"exclude"`
	Keep                     Keep     `yaml:"keep"`
	RsyncExtraFlags          string   `yaml:"rsync_extra_flags"`
	IgnoreVanishedFilesError bool     `yaml:"ignore_vanished_files_error"`
}

type Keep struct {
	Daily   int `yaml:"daily"`
	Weekly  int `yaml:"weekly"`
	Monthly int `yaml:"monthly"`
}

func main() {
	flag.Parse()

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC1123Z})

	config, err := readConfig(*configFile)
	if err != nil {
		log.Fatal().Err(err).Msg("error reading config")
	}

	if err := runBackup(config, *dryRun); err != nil {
		log.Fatal().Err(err).Msg("backup failed")
	}

	if err := purgeBackups(config, *dryRun); err != nil {
		log.Fatal().Err(err).Msg("purging old backups failed")
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

var execCommand = exec.Command

func runBackup(config *Config, dryRun bool) error {
	log.Info().Strs("source", config.Source).Str("destination", config.Destination).Msg("Snapshot")

	unfinishedDir := filepath.Join(config.Destination, ".unfinished")
	snapshotName := fmt.Sprintf("%s_%s", config.SnapshotPrefix, time.Now().Format("2006-01-02_15:04:05"))
	finalDest := filepath.Join(config.Destination, snapshotName)

	if !dryRun {
		log.Info().Str("path", unfinishedDir).Msg("Removing temporary directory if it exists")
		if err := os.RemoveAll(unfinishedDir); err != nil {
			return fmt.Errorf("failed to remove unfinished directory: %w", err)
		}
		log.Info().Str("path", unfinishedDir).Msg("Creating temporary directory")
		if err := os.MkdirAll(unfinishedDir, 0755); err != nil {
			return fmt.Errorf("failed to create unfinished directory: %w", err)
		}
	} else {
		log.Info().Str("path", unfinishedDir).Msg("[Dry Run] Would remove temporary directory if it exists")
		log.Info().Str("path", unfinishedDir).Msg("[Dry Run] Would create temporary directory")
	}

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

	cmd := execCommand("rsync", args...)
	log.Info().Str("command", fmt.Sprintf("rsync %s", strings.Join(args, " "))).Msg("Running command")

	if dryRun {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		logFile, err := os.Create(filepath.Join(unfinishedDir, "rsync.log"))

		errorTee := io.MultiWriter(os.Stderr, logFile)

		if err != nil {
			return fmt.Errorf("failed to create rsync log file: %w", err)
		}
		//nolint:errcheck
		defer logFile.Close()
		cmd.Stdout = logFile
		cmd.Stderr = errorTee
	}

	if err := cmd.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			if exitError.ExitCode() == 24 && config.IgnoreVanishedFilesError {
				log.Warn().Msg("rsync completed with exit code 24, but ignoring due to configuration.")
			} else {
				return fmt.Errorf("rsync command failed: %w", err)
			}
		} else {
			return fmt.Errorf("rsync command failed: %w", err)
		}
	}

	if !dryRun {
		log.Info().Str("from", unfinishedDir).Str("to", finalDest).Msg("Renaming temporary directory")
		if err := os.Rename(unfinishedDir, finalDest); err != nil {
			return fmt.Errorf("failed to rename unfinished directory: %w", err)
		}
	} else {
		log.Info().Str("from", unfinishedDir).Str("to", finalDest).Msg("[Dry Run] Would rename")
	}

	log.Info().Msg("Backup finished successfully")
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

func purgeBackups(config *Config, dryRun bool) error {
	snapshots, err := getSnapshots(config.Destination) // getSnapshots sorts oldest to newest
	if err != nil {
		return err
	}
	// Reverse to sort newest to oldest
	for i, j := 0, len(snapshots)-1; i < j; i, j = i+1, j-1 {
		snapshots[i], snapshots[j] = snapshots[j], snapshots[i]
	}

	log.Info().Int("count", len(snapshots)).Msg("Found snapshots to consider for purging.")
	if len(snapshots) == 0 {
		log.Info().Msg("No snapshots found to purge.")
		return nil
	}

	to_keep := make(map[string]bool)

	// Daily backups
	daily_kept_count := 0
	for i := 0; i < len(snapshots) && daily_kept_count < config.Keep.Daily; i++ {
		s := snapshots[i]
		if !to_keep[s.Name()] {
			log.Info().Str("snapshot", s.Name()).Msg("Keeping snapshot as a daily backup.")
			to_keep[s.Name()] = true
			daily_kept_count++
		}
	}

	// Weekly backups
	weekly_kept_count := 0
	weeks_seen := make(map[int]bool)
	for _, s := range snapshots {
		if weekly_kept_count >= config.Keep.Weekly {
			break
		}
		year, week := s.ModTime().ISOWeek()
		week_key := year*100 + week
		if !weeks_seen[week_key] {
			weeks_seen[week_key] = true
			if !to_keep[s.Name()] {
				log.Info().Str("snapshot", s.Name()).Msg("Keeping snapshot as a weekly backup.")
				to_keep[s.Name()] = true
				weekly_kept_count++
			}
		}
	}

	// Monthly backups
	monthly_kept_count := 0
	months_seen := make(map[int]bool)
	for _, s := range snapshots {
		if monthly_kept_count >= config.Keep.Monthly {
			break
		}
		year, month, _ := s.ModTime().Date()
		month_key := year*100 + int(month)
		if !months_seen[month_key] {
			months_seen[month_key] = true
			if !to_keep[s.Name()] {
				log.Info().Str("snapshot", s.Name()).Msg("Keeping snapshot as a monthly backup.")
				to_keep[s.Name()] = true
				monthly_kept_count++
			}
		}
	}

	log.Info().Msg("--- Purge Summary ---")
	for _, s := range snapshots {
		if !to_keep[s.Name()] {
			if dryRun {
				log.Info().Str("path", filepath.Join(config.Destination, s.Name())).Msg("[Dry Run] Would purge snapshot directory")
			} else {
				log.Info().Str("snapshot", s.Name()).Msg("Purging snapshot")
				err := os.RemoveAll(filepath.Join(config.Destination, s.Name()))
				if err != nil {
					log.Error().Err(err).Str("snapshot", s.Name()).Msg("Failed to purge snapshot")
				}
			}
		}
	}
	log.Info().Msg("--- End Purge Summary ---")

	return nil
}
