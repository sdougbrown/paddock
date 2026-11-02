package bwrap

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

// prepareHome lays out the throwaway HOME the run binds at its host path:
// the .claude directory, the onboarding seed, the managed settings, the
// seed files, and the skills and agents symlinks. The layout mirrors what
// paddock-init does for the container engines; everything runs as the
// invoking user, so there is no ownership work.
func prepareHome(home string, claude plan.ClaudePlan, cfg *initcfg.Config, tdir string) error {
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		return fmt.Errorf("home: %w", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), cfg.Claude.ClaudeJSON, 0o600); err != nil {
		return fmt.Errorf("home: %w", err)
	}
	if len(claude.ManagedSettings) > 0 {
		if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), claude.ManagedSettings, 0o644); err != nil {
			return fmt.Errorf("home: %w", err)
		}
	}
	seen := map[string]bool{}
	for _, f := range claude.SeedFiles {
		base := filepath.Base(f)
		if seen[base] {
			return fmt.Errorf("claude.seed_files: two files share the name %s", base)
		}
		seen[base] = true
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("claude.seed_files: %w", err)
		}
		if err := os.WriteFile(filepath.Join(home, ".claude", base), data, 0o644); err != nil {
			return fmt.Errorf("claude.seed_files: %w", err)
		}
	}
	for name, target := range map[string]string{
		"skills": claude.Skills,
		"agents": claude.Agents,
	} {
		if target == "" {
			continue
		}
		link := filepath.Join(home, ".claude", name)
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("claude %s symlink: %w", name, err)
		}
	}
	if tdir != "" {
		if err := os.MkdirAll(tdir, 0o755); err != nil {
			return fmt.Errorf("transcript dir: %w", err)
		}
		// The transcript directory must be writable from the namespace even
		// though it sits under the read-only host rootfs, so the home's
		// projects path points at it; the bind in runArgv carries the writes
		// back to the host path avenor reads.
		if err := os.Symlink(tdir, filepath.Join(home, ".claude", "projects")); err != nil {
			return fmt.Errorf("transcript dir: %w", err)
		}
	}
	return nil
}
