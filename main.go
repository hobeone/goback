package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Mode                     string   `yaml:"mode"`
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

type App struct {
	config  *Config
	dryRun  bool
	execCmd func(string, ...string) *exec.Cmd
}

func main() {
	dryRun := flag.Bool("dry-run", false, "print actions without executing them")
	configFile := flag.String("config", "config.yaml", "path to the configuration file")
	flag.Parse()

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC1123Z})

	config, err := readConfig(*configFile)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to read config")
	}

	app := &App{
		config:  config,
		dryRun:  *dryRun,
		execCmd: exec.Command,
	}

	if err := app.Run(); err != nil {
		log.Fatal().Err(err).Msg("backup failed")
	}
}

func (a *App) Run() error {
	switch a.config.Mode {
	case "", "snapshot":
		if err := a.runSnapshotBackup(); err != nil {
			return fmt.Errorf("snapshot backup failed: %w", err)
		}
		if err := a.purgeBackups(); err != nil {
			return fmt.Errorf("purging old backups failed: %w", err)
		}
	case "simple":
		if err := a.runSimpleBackup(); err != nil {
			return fmt.Errorf("simple backup failed: %w", err)
		}
	default:
		return fmt.Errorf("invalid backup mode: %s", a.config.Mode)
	}
	return nil
}

func readConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

func (a *App) runSnapshotBackup() error {
	log.Info().Strs("source", a.config.Source).Str("destination", a.config.Destination).Msg("starting snapshot backup")

	unfinishedDir := filepath.Join(a.config.Destination, ".unfinished")
	snapshotName := fmt.Sprintf("%s_%s", a.config.SnapshotPrefix, time.Now().Format(snapshotTimeLayout))
	finalDest := filepath.Join(a.config.Destination, snapshotName)

	if !a.dryRun {
		log.Info().Str("path", unfinishedDir).Msg("removing temporary directory if it exists")
		if err := os.RemoveAll(unfinishedDir); err != nil {
			return fmt.Errorf("failed to remove unfinished directory: %w", err)
		}
		log.Info().Str("path", unfinishedDir).Msg("creating temporary directory")
		if err := os.MkdirAll(unfinishedDir, 0755); err != nil {
			return fmt.Errorf("failed to create unfinished directory: %w", err)
		}
	} else {
		log.Info().Str("path", unfinishedDir).Msg("[dry run] would remove temporary directory if it exists")
		log.Info().Str("path", unfinishedDir).Msg("[dry run] would create temporary directory")
	}

	latestSnapshot, err := a.getLatestSnapshot()
	if err != nil {
		return fmt.Errorf("failed to get latest snapshot: %w", err)
	}

	linkDest := ""
	if latestSnapshot != "" {
		linkDest = filepath.Join(a.config.Destination, latestSnapshot)
	}

	if err := a.runRsync(unfinishedDir, linkDest); err != nil {
		return err
	}

	if !a.dryRun {
		log.Info().Str("from", unfinishedDir).Str("to", finalDest).Msg("renaming temporary directory")
		if err := os.Rename(unfinishedDir, finalDest); err != nil {
			return fmt.Errorf("failed to rename unfinished directory: %w", err)
		}
	} else {
		log.Info().Str("from", unfinishedDir).Str("to", finalDest).Msg("[dry run] would rename temporary directory")
	}

	log.Info().Msg("snapshot backup finished successfully")
	return nil
}

func (a *App) runSimpleBackup() error {
	log.Info().Strs("source", a.config.Source).Str("destination", a.config.Destination).Msg("starting simple backup")

	if !a.dryRun {
		if err := os.MkdirAll(a.config.Destination, 0755); err != nil {
			return fmt.Errorf("failed to create destination directory: %w", err)
		}
	}

	if err := a.runRsync(a.config.Destination, ""); err != nil {
		return err
	}

	log.Info().Msg("simple backup finished successfully")
	return nil
}

func (a *App) runRsync(destDir string, linkDest string) (retErr error) {
	args := []string{"-a", "-v", "-h", "--delete", "--stats", "--inplace"}
	if linkDest != "" {
		args = append(args, "--link-dest="+linkDest)
	}
	for _, ex := range a.config.Exclude {
		args = append(args, "--exclude="+ex)
	}
	if a.config.RsyncExtraFlags != "" {
		args = append(args, strings.Fields(a.config.RsyncExtraFlags)...)
	}

	if a.dryRun {
		if !slices.Contains(args, "--dry-run") && !slices.Contains(args, "-n") {
			args = append(args, "--dry-run")
		}
	}

	args = append(args, a.config.Source...)
	args = append(args, destDir)

	cmd := a.execCmd("rsync", args...)
	log.Info().Str("command", fmt.Sprintf("rsync %s", strings.Join(args, " "))).Msg("running rsync")

	if a.dryRun {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		var logWriter io.Writer
		if a.config.Mode == "simple" {
			logWriter = os.Stdout
		} else {
			logFile, err := os.Create(filepath.Join(destDir, "rsync.log"))
			if err != nil {
				return fmt.Errorf("failed to create rsync log file: %w", err)
			}
			// A failed Close can mean buffered log data was lost, so report it
			// unless rsync itself already failed.
			defer func() {
				if cerr := logFile.Close(); cerr != nil && retErr == nil {
					retErr = fmt.Errorf("failed to close rsync log file: %w", cerr)
				}
			}()
			logWriter = logFile
		}

		cmd.Stdout = logWriter
		cmd.Stderr = io.MultiWriter(os.Stderr, logWriter)
	}

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 24 && a.config.IgnoreVanishedFilesError {
				log.Warn().Msg("rsync completed with exit code 24 (vanished files), ignoring per config")
				return nil
			}
		}
		return fmt.Errorf("rsync command failed: %w", err)
	}

	return nil
}

// snapshotTimeLayout is the timestamp format embedded in snapshot names,
// which have the form <prefix>_<timestamp> in local time.
const snapshotTimeLayout = "2006-01-02_15:04:05"

type snapshot struct {
	name string
	time time.Time // creation time, parsed from name
}

// parseSnapshotName reports whether name is a snapshot created with prefix
// and, if so, the creation time encoded in it.
func parseSnapshotName(name, prefix string) (time.Time, bool) {
	stamp, ok := strings.CutPrefix(name, prefix+"_")
	if !ok {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(snapshotTimeLayout, stamp, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// getSnapshots returns the snapshots in dest, oldest first. Only directories
// named <prefix>_<timestamp> count. Anything else (lost+found, snapshots
// made under a different prefix, unrelated data) is ignored, so it is never
// used as --link-dest and never purged.
//
// Order comes from the name rather than the directory mtime because rsync -a
// copies the source directory's mtime onto the snapshot root when a source
// path ends in a slash.
func getSnapshots(dest, prefix string) ([]snapshot, error) {
	entries, err := os.ReadDir(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var snapshots []snapshot
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		t, ok := parseSnapshotName(entry.Name(), prefix)
		if !ok {
			log.Info().Str("dir", entry.Name()).Str("prefix", prefix).Msg("ignoring directory that is not a snapshot")
			continue
		}
		snapshots = append(snapshots, snapshot{name: entry.Name(), time: t})
	}

	slices.SortFunc(snapshots, func(a, b snapshot) int {
		return a.time.Compare(b.time)
	})

	return snapshots, nil
}

func (a *App) getLatestSnapshot() (string, error) {
	snapshots, err := getSnapshots(a.config.Destination, a.config.SnapshotPrefix)
	if err != nil || len(snapshots) == 0 {
		return "", err
	}
	return snapshots[len(snapshots)-1].name, nil
}

func (a *App) purgeBackups() error {
	snapshots, err := getSnapshots(a.config.Destination, a.config.SnapshotPrefix)
	if err != nil {
		return err
	}

	slices.Reverse(snapshots)

	log.Info().Int("count", len(snapshots)).Msg("found snapshots to consider for purging")
	if len(snapshots) == 0 {
		return nil
	}

	toKeep := make(map[string]bool)

	// Daily backups
	dailyCount := 0
	for _, s := range snapshots {
		if dailyCount >= a.config.Keep.Daily {
			break
		}
		if !toKeep[s.name] {
			log.Info().Str("snapshot", s.name).Msg("keeping as daily backup")
			toKeep[s.name] = true
			dailyCount++
		}
	}

	// Weekly backups
	weeklyCount := 0
	weeksSeen := make(map[int]bool)
	for _, s := range snapshots {
		if weeklyCount >= a.config.Keep.Weekly {
			break
		}
		year, week := s.time.ISOWeek()
		weekKey := year*100 + week
		if !weeksSeen[weekKey] {
			weeksSeen[weekKey] = true
			if !toKeep[s.name] {
				log.Info().Str("snapshot", s.name).Msg("keeping as weekly backup")
				toKeep[s.name] = true
				weeklyCount++
			}
		}
	}

	// Monthly backups
	monthlyCount := 0
	monthsSeen := make(map[int]bool)
	for _, s := range snapshots {
		if monthlyCount >= a.config.Keep.Monthly {
			break
		}
		year, month, _ := s.time.Date()
		monthKey := year*100 + int(month)
		if !monthsSeen[monthKey] {
			monthsSeen[monthKey] = true
			if !toKeep[s.name] {
				log.Info().Str("snapshot", s.name).Msg("keeping as monthly backup")
				toKeep[s.name] = true
				monthlyCount++
			}
		}
	}

	log.Info().Msg("--- purge summary ---")
	for _, s := range snapshots {
		if !toKeep[s.name] {
			path := filepath.Join(a.config.Destination, s.name)
			if a.dryRun {
				log.Info().Str("path", path).Msg("[dry run] would purge snapshot")
			} else {
				log.Info().Str("snapshot", s.name).Msg("purging snapshot")
				if err := os.RemoveAll(path); err != nil {
					log.Error().Err(err).Str("snapshot", s.name).Msg("failed to purge snapshot")
				}
			}
		}
	}
	log.Info().Msg("--- end purge summary ---")

	return nil
}
