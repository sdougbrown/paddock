package applecontainer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sdougbrown/paddock/internal/initcfg"
	"github.com/sdougbrown/paddock/internal/plan"
	"github.com/sdougbrown/paddock/internal/resolve"
)

// runDirs are the per-run host directories under <state root>/runs/<name>.
// State, Secrets, and Settings are removed when the run ends; Log is kept.
type runDirs struct {
	Root, State, Secrets, Log, Settings string
}

func newRunName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "paddock-" + hex.EncodeToString(b[:]), nil
}

func createRunDirs(stateRoot, name string) (runDirs, error) {
	root := filepath.Join(stateRoot, "runs", name)
	d := runDirs{
		Root:    root,
		State:   filepath.Join(root, "state"),
		Secrets: filepath.Join(root, "secrets"),
		Log:     filepath.Join(root, "log"),
		// A file mount shares its parent directory, so the settings file
		// gets a directory of its own.
		Settings: filepath.Join(root, "settings"),
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return runDirs{}, err
	}
	// Mkdir fails if the run directory exists, so two runs never share one.
	if err := os.Mkdir(root, 0o700); err != nil {
		return runDirs{}, err
	}
	for _, dir := range []string{filepath.Dir(root), root, d.State, d.Secrets, d.Log, d.Settings} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return runDirs{}, err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return runDirs{}, err
		}
	}
	return d, nil
}

// removable lists the directories that hold per-run secrets and config.
func (d runDirs) removable() []string {
	return []string{d.State, d.Secrets, d.Settings}
}

// remove deletes the removable directories.
func (d runDirs) remove() {
	for _, dir := range d.removable() {
		os.RemoveAll(dir)
	}
}

// copySeedFiles copies the plan's seed files into <state>/seed.
func copySeedFiles(state string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	dir := filepath.Join(state, filepath.Base(initcfg.SeedDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	seen := map[string]string{}
	for _, f := range files {
		base := filepath.Base(f)
		if prev, dup := seen[base]; dup {
			return fmt.Errorf("claude.seed_files: %s and %s share the name %s", prev, f, base)
		}
		seen[base] = f
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("claude.seed_files: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, base), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// resolveSecrets reads every non-literal env value. The result is KEY=VALUE
// lines for secrets.env, or nil when the plan has no such values. Errors never
// include a value.
func resolveSecrets(env []plan.EnvVar, lookup func(string) (string, bool)) ([]byte, error) {
	var b strings.Builder
	_, refs := splitEnv(env)
	for _, e := range refs {
		v, err := resolve.ResolveValue(e, lookup)
		if err != nil {
			return nil, err
		}
		b.WriteString(e.Name + "=" + v + "\n")
	}
	if b.Len() == 0 {
		return nil, nil
	}
	return []byte(b.String()), nil
}

var secretsFile = filepath.Base(initcfg.SecretsPath)

func writeSecrets(dir string, data []byte) error {
	return os.WriteFile(filepath.Join(dir, secretsFile), data, 0o600)
}

// stageInitBinary copies the paddock-init binary at src into
// <cache>/bin/<sha256>/paddock-init, so the guest mount exposes only that
// file, and returns the directory.
func stageInitBinary(cacheRoot, src string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("paddock-init binary: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	dir := filepath.Join(cacheRoot, "bin", hex.EncodeToString(h.Sum(nil)))
	dst := filepath.Join(dir, filepath.Base(initcfg.InitPath))
	if _, err := os.Stat(dst); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".paddock-init-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, f); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	return dir, os.Rename(tmp.Name(), dst)
}

// locateInitBinary returns $PADDOCK_INIT, else paddock-init beside the running executable.
func locateInitBinary(getenv func(string) string, executable func() (string, error)) (string, error) {
	if p := getenv("PADDOCK_INIT"); p != "" {
		return p, nil
	}
	exe, err := executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "paddock-init"), nil
}
