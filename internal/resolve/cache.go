package resolve

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/sdougbrown/paddock/internal/plan"
)

// CacheDir returns the host dependency-cache directory for a spec and
// workdir: <home>/.cache/paddock/<ref as nested directories>/<first 16 hex of
// sha256 of the workdir's keyFile>. home must be absolute. Refs are validated
// against ".." when the spec chain loads.
func CacheDir(home, ref, workdir, keyFile string) (string, error) {
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("setup cache: home directory %q is not absolute", home)
	}
	sum, err := hashKeyFile(workdir, keyFile)
	if err != nil {
		return "", fmt.Errorf("setup.cache_key: %w", err)
	}
	return filepath.Join(home, ".cache", "paddock", filepath.FromSlash(ref), hex.EncodeToString(sum)[:16]), nil
}

// maxKeyFileSize bounds how much of a cache_key file CacheDir reads.
const maxKeyFileSize = 64 << 20

// hashKeyFile returns the sha256 of workdir's keyFile. The workspace controls
// that file, so its symlinks must resolve inside workdir, it is opened through
// an os.Root without blocking, and it must be a regular file of at most
// maxKeyFileSize bytes.
func hashKeyFile(workdir, keyFile string) ([]byte, error) {
	dir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	rel, ok := relInside(dir, real)
	if !ok {
		return nil, fmt.Errorf("%q resolves outside the workdir", keyFile)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return nil, err
	} else if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", keyFile)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return nil, err
	}
	if n > maxKeyFileSize {
		return nil, fmt.Errorf("%q is larger than %d bytes", keyFile, maxKeyFileSize)
	}
	return h.Sum(nil), nil
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
// under the other: containment checked in both directions.
func guestOverlap(a, b string) bool {
	_, aInB := relInside(b, a)
	_, bInA := relInside(a, b)
	return aInB || bInA
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
