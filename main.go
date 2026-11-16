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

// validate rejects retention settings that would make purge delete every
// snapshot, including the one just taken. With any count above zero the
// newest snapshot always survives, since it claims its own day, week and
// month.
func (k Keep) validate() error {
	if k.Daily < 0 || k.Weekly < 0 || k.Monthly < 0 {
		return fmt.Errorf("keep counts must not be negative (daily=%d weekly=%d monthly=%d)", k.Daily, k.Weekly, k.Monthly)
	}
	if k.Daily == 0 && k.Weekly == 0 && k.Monthly == 0 {
		return errors.New("keep: at least one of daily, weekly or monthly must be greater than 0, otherwise every snapshot would be purged")
	}
	return nil
}

// validate checks everything Run relies on before anything touches disk,
// reporting every problem at once.
func (c *Config) validate() error {
	var errs []error

	snapshotMode := false
	switch c.Mode {
	case "", "snapshot":
		snapshotMode = true
		if err := c.Keep.validate(); err != nil {
			errs = append(errs, err)
		}
	case "simple":
	default:
		errs = append(errs, fmt.Errorf("invalid backup mode: %q", c.Mode))
	}

	if c.Destination == "" {
		errs = append(errs, errors.New("destination must be set"))
	} else if snapshotMode {
		// Never create the destination in snapshot mode: if a backup drive
		// is not mounted, its mount point is an empty directory (or missing)
		// on the root filesystem, and backing up there can fill it.
		info, err := os.Stat(c.Destination)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("destination %q must already exist (is the backup drive mounted?): %w", c.Destination, err))
		case !info.IsDir():
			errs = append(errs, fmt.Errorf("destination %q is not a directory", c.Destination))
		}
	}

	if len(c.Source) == 0 {
		errs = append(errs, errors.New("source must list at least one path"))
	}
	for i, src := range c.Source {
		if strings.TrimSpace(src) == "" {
			errs = append(errs, fmt.Errorf("source[%d] is empty", i))
			continue
		}
		if _, err := os.Stat(src); err != nil {
			errs = append(errs, fmt.Errorf("source %q: %w", src, err))
			continue
		}
		if c.Destination != "" && isWithin(c.Destination, src) {
			errs = append(errs, fmt.Errorf("destination %q is inside source %q, so every backup would include all previous ones", c.Destination, src))
		}
	}

	return errors.Join(errs...)
}

// isWithin reports whether path is dir or lies underneath it. Both are made
// absolute and have symlinks resolved where they exist, so "..", relative
// paths and symlinked sources cannot hide the overlap.
func isWithin(path, dir string) bool {
	rel, err := filepath.Rel(resolvePath(dir), resolvePath(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolvePath returns p as an absolute path with symlinks evaluated. For a
// path that does not exist yet it resolves the nearest existing parent, so a
// not-yet-created directory under a symlink still compares correctly.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	var missing []string
	for {
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			return filepath.Join(append([]string{real}, missing...)...)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return filepath.Join(append([]string{abs}, missing...)...)
		}
		missing = append([]string{filepath.Base(abs)}, missing...)
		abs = parent
	}
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
	if err := a.config.validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
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
	snapshotName := snapshotDirName(a.config.SnapshotPrefix, time.Now())
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

// Snapshot directories are named <prefix><snapshotSep><timestamp>, with the
// timestamp in local time. snapshotDirName writes that format and
// parseSnapshotName reads it.
//
// The layout avoids ':', which SMB, exFAT and Windows filesystems reject.
// Snapshots made before that change used legacySnapshotTimeLayout; they are
// still recognised so upgrading neither orphans them (forcing a full copy
// and leaving them unpurged) nor misorders them against new snapshots.
const (
	snapshotSep              = "_"
	snapshotTimeLayout       = "2006-01-02_15-04-05"
	legacySnapshotTimeLayout = "2006-01-02_15:04:05"
)

// snapshotDirName returns the directory name for a snapshot taken at t.
func snapshotDirName(prefix string, t time.Time) string {
	return prefix + snapshotSep + t.Format(snapshotTimeLayout)
}

type snapshot struct {
	name string
	time time.Time // creation time, parsed from name
}

// parseSnapshotName reports whether name is a snapshot created with prefix
// and, if so, the creation time encoded in it.
func parseSnapshotName(name, prefix string) (time.Time, bool) {
	stamp, ok := strings.CutPrefix(name, prefix+snapshotSep)
	if !ok {
		return time.Time{}, false
	}
	for _, layout := range []string{snapshotTimeLayout, legacySnapshotTimeLayout} {
		if t, err := time.ParseInLocation(layout, stamp, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
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
		// Dot directories (notably .unfinished) are never snapshots; skipping
		// them here keeps them out of the "ignoring" log below.
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
