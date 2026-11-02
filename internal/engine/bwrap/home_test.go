package bwrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
)

// TestPrepareHome checks the seeded throwaway home: the onboarding marker,
// the managed settings, the seed files with a duplicate-name rejection, and
// the skills, agents, and projects symlinks.
func TestPrepareHome(t *testing.T) {
	home := t.TempDir()
	seed := filepath.Join(home, "seed-src")
	if err := os.WriteFile(seed, []byte("context"), 0o644); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(home, "skills-src")
	if err := os.Mkdir(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	tdir := filepath.Join(home, ".claude", "projects", "enc")
	p := plan.Plan{
		Claude: plan.ClaudePlan{
			ManagedSettings: []byte(`{"deny":true}`),
			SeedFiles:       []string{seed},
			Skills:          skills,
			Agents:          "",
		},
	}
	cfg := &initcfg.Config{Claude: initcfg.ClaudeHome{ClaudeJSON: []byte(`{"hasCompletedOnboarding":true}`)}}
	if err := prepareHome(filepath.Join(home, "throwaway"), p.Claude, cfg, tdir); err != nil {
		t.Fatalf("prepareHome: %v", err)
	}
	h := filepath.Join(home, "throwaway")
	if b, err := os.ReadFile(filepath.Join(h, ".claude.json")); err != nil || string(b) != `{"hasCompletedOnboarding":true}` {
		t.Errorf(".claude.json = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(h, ".claude", "settings.json")); err != nil || string(b) != `{"deny":true}` {
		t.Errorf("settings.json = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(h, ".claude", "seed-src")); err != nil || string(b) != "context" {
		t.Errorf("seed file = %q, %v", b, err)
	}
	for name, want := range map[string]string{"skills": skills, "projects": tdir} {
		if target, err := os.Readlink(filepath.Join(h, ".claude", name)); err != nil || target != want {
			t.Errorf("%s symlink = %q, %v; want %q", name, target, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(h, ".claude", "agents")); !os.IsNotExist(err) {
		t.Errorf("agents symlink exists without a target: %v", err)
	}
	// A second seed file with the same base name is rejected, not overwritten.
	dup := filepath.Join(home, "dup", "seed-src")
	if err := os.MkdirAll(filepath.Dir(dup), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dup, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Claude.SeedFiles = append(p.Claude.SeedFiles, dup)
	err := prepareHome(filepath.Join(home, "throwaway2"), p.Claude, cfg, "")
	if err == nil || !strings.Contains(err.Error(), "share the name") {
		t.Errorf("duplicate seed file: err = %v; want a shared-name error", err)
	}
}
