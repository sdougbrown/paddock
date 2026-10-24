package resolve

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/sdougbrown/paddock/internal/plan"
)

// CacheDir returns the host dependency-cache directory for a spec and
// workdir: <home>/.cache/paddock/<lowercased ref as nested directories>/<first
// 16 hex of sha256 of the workdir's keyFile>. Refs are validated against ".."
// when the spec chain loads.
func CacheDir(home, ref, workdir, keyFile string) (string, error) {
	data, err := os.ReadFile(filepath.Join(workdir, keyFile))
	if err != nil {
		return "", fmt.Errorf("setup.cache_key: %w", err)
	}
	sum := sha256.Sum256(data)
	return filepath.Join(home, ".cache", "paddock", filepath.FromSlash(strings.ToLower(ref)), hex.EncodeToString(sum[:])[:16]), nil
}

// setupCache adds the dependency-cache mount to a setup-phase plan. The
// cache_key file resolves against the plan's workdir, where the setup command
// runs. A spec that sets cache_dir needs cache_key, and the reverse.
func (r *Resolver) setupCache(p *plan.Plan) error {
	s := p.Setup
	if s.CacheDir == "" && s.CacheKeyFile == "" {
		return nil
	}
	if s.CacheDir == "" || s.CacheKeyFile == "" {
		return fmt.Errorf("setup: cache_dir and cache_key must be set together in spec %q", p.Spec)
	}
	host, err := CacheDir(r.Home, p.Spec, p.Workdir, s.CacheKeyFile)
	if err != nil {
		return err
	}
	for _, m := range p.Mounts {
		if guestOverlap(s.CacheDir, m.Guest) {
			return fmt.Errorf("setup.cache_dir %q overlaps the mount at %q", s.CacheDir, m.Guest)
		}
	}
	s.CacheHost = host
	p.Mounts = append(p.Mounts, plan.Mount{Host: host, Guest: s.CacheDir})
	return nil
}

// guestOverlap reports whether guest paths a and b are equal or one lies
// under the other.
func guestOverlap(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}

// completionMarker is the host file recording that setup finished in s's
// cache. It sits beside the cache directory, outside the guest mount.
func completionMarker(s *plan.SetupPlan) string { return s.CacheHost + ".complete" }

// lockFile is the host file whose exclusive flock serializes setup runs for
// one cache. It sits beside the cache directory, outside the guest mount.
func lockFile(s *plan.SetupPlan) string { return s.CacheHost + ".lock" }

// PrepareSetupCache takes an exclusive lock on the cache, so setups for the
// same cache run one at a time, then creates the host cache directory,
// reports whether an earlier setup run completed in it, and removes the
// completion marker so only a run that finishes from here on can set it. The
// lock is held until the returned release func is called; release is safe to
// call more than once and is never nil. A plan without a cache is a miss and
// takes no lock.
func PrepareSetupCache(s *plan.SetupPlan) (hit bool, release func(), err error) {
	release = func() {}
	if s.CacheHost == "" {
		return false, release, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.CacheHost), 0o755); err != nil {
		return false, release, err
	}
	lf, err := os.OpenFile(lockFile(s), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, release, fmt.Errorf("lock setup cache: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		lf.Close()
		return false, release, fmt.Errorf("lock setup cache: %w", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { lf.Close() }) }
	marker := completionMarker(s)
	if st, err := os.Lstat(marker); err == nil && st.Mode().IsRegular() {
		hit = true
	}
	if err := os.MkdirAll(s.CacheHost, 0o755); err != nil {
		release()
		return false, func() {}, err
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		release()
		return false, func() {}, err
	}
	return hit, release, nil
}

// CompleteSetupCache records that setup finished successfully. It creates the
// marker exclusively and without following a symlink.
func CompleteSetupCache(s *plan.SetupPlan) error {
	if s.CacheHost == "" {
		return nil
	}
	f, err := os.OpenFile(completionMarker(s), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return fmt.Errorf("mark setup cache complete: %w", err)
	}
	return f.Close()
}
