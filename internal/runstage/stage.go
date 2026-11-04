package runstage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

// Staged carries what staging produced for an engine: the transcript
// directory it created, the built init config, and the host path of the
// staged managed settings (empty when the plan has none).
type Staged struct {
	Cfg      *initcfg.Config
	Settings string
	Pass     []string // the passthrough environment lines
}

// StagedSettings is the host path of a run's staged managed settings.
func StagedSettings(dirs RunDirs) string {
	return filepath.Join(dirs.Settings, UserSettingsFile)
}

// StagedSeedDir is the host path of a run's staged seed files.
func StagedSeedDir(dirs RunDirs) string {
	return filepath.Join(dirs.State, filepath.Base(initcfg.SeedDir))
}

// Stage performs the staging both engines share: it creates the run's
// transcript directory (0700 — it holds agent sessions), builds the
// passthrough environment from getenv, writes the managed settings beside
// the config, copies the seed files, and builds the init config. home is
// the host home the transcript dir lives under. cfgTranscriptDir is what
// the config records for the transcript dir (validated with everything
// else): the container engines set the writable dir itself, the bwrap
// engine the projects symlink target (its parent). mounts feed the
// config's mount list; engines that keep mounts out of the config pass
// nil.
func Stage(p *plan.Plan, u initcfg.User, getenv func(string) string, home, cfgTranscriptDir string, dirs RunDirs, mounts []GuestMount, verify bool) (Staged, error) {
	tdir := TranscriptDir(home, p.Workdir)
	// 0700: on a shared host the transcript dir holds agent sessions, and
	// MkdirAll's mode applies to the ~/.claude and ~/.claude/projects it
	// creates on the way down.
	if err := os.MkdirAll(tdir, 0o700); err != nil {
		return Staged{}, fmt.Errorf("transcript dir: %w", err)
	}
	var pass []string
	for _, k := range PassthroughEnv {
		if v := getenv(k); v != "" {
			pass = append(pass, k+"="+v)
		}
	}
	cfg, err := BuildInitConfig(p, InitInput{User: u, Verify: verify, TranscriptDir: cfgTranscriptDir, Mounts: mounts, Passthrough: pass})
	if err != nil {
		return Staged{}, err
	}
	var settings string
	if len(p.Claude.ManagedSettings) > 0 {
		settings = StagedSettings(dirs)
		if err := os.WriteFile(settings, p.Claude.ManagedSettings, 0o644); err != nil {
			return Staged{}, err
		}
	}
	if err := CopySeedFiles(dirs, p.Claude.SeedFiles); err != nil {
		return Staged{}, err
	}
	return Staged{Cfg: cfg, Settings: settings, Pass: pass}, nil
}
