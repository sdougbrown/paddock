package pinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"

	"github.com/sdougbrown/paddock/internal/initcfg"
)

// homeFS is the filesystem prepareHome works on. Guest paths are joined
// onto root; chown is os.Lchown in the guest; mounts lists guest mount
// points, which are never created, chowned, or replaced.
type homeFS struct {
	root   string
	chown  func(path string, uid, gid int) error
	mounts map[string]bool
}

func newHomeFS(root string, cfg *initcfg.Config) homeFS {
	h := homeFS{root: root, chown: os.Lchown, mounts: map[string]bool{}}
	for _, m := range cfg.Mounts {
		h.mounts[path.Clean(m.Path)] = true
	}
	if t := cfg.Claude.TranscriptDir; t != "" {
		h.mounts[path.Clean(t)] = true
	}
	return h
}

func (h homeFS) p(guest string) string { return filepath.Join(h.root, guest) }

// isMount reports whether guest is a listed mount point or sits on a
// different device from its parent.
func (h homeFS) isMount(guest string) bool {
	if h.mounts[guest] {
		return true
	}
	st, err1 := os.Lstat(h.p(guest))
	pst, err2 := os.Lstat(h.p(path.Dir(guest)))
	if err1 != nil || err2 != nil {
		return false
	}
	a, ok1 := st.Sys().(*syscall.Stat_t)
	b, ok2 := pst.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && a.Dev != b.Dev
}

// agentDir makes guest exist as a directory owned by the agent, and refuses
// the filesystem root. An existing mount point is left alone. Missing
// parents are created root-owned 0755.
func (h homeFS) agentDir(guest string, mode os.FileMode, uid, gid int) error {
	if path.Clean(guest) == "/" {
		return errors.New("refusing to take ownership of the filesystem root")
	}
	if h.isMount(guest) {
		return nil
	}
	if err := os.MkdirAll(h.p(path.Dir(guest)), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(h.p(guest), mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	st, err := os.Lstat(h.p(guest))
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", guest)
	}
	return h.chown(h.p(guest), uid, gid)
}

// agentDirSpec is a directory prepareHome makes agent-owned.
type agentDirSpec struct {
	path string
	mode os.FileMode
}

// agentDirs lists the directories under home that the agent owns and may
// write: home itself and ~/.claude.
func agentDirs(home string) []agentDirSpec {
	return []agentDirSpec{{home, 0o755}, {home + "/.claude", 0o700}}
}

// prepareHome lays out the agent's HOME: HOME and ~/.claude (and
// ~/.claude/projects when it already exists) owned by the agent, seed files
// and ~/.claude.json, the skills and agents symlinks, and the managed
// settings file.
func prepareHome(h homeFS, cfg *initcfg.Config) error {
	u, c := cfg.User, cfg.Claude
	home := path.Clean(u.Home)
	claude := home + "/.claude"
	for _, d := range agentDirs(home) {
		if err := h.agentDir(d.path, d.mode, u.UID, u.GID); err != nil {
			return fmt.Errorf("home: %w", err)
		}
	}
	if _, err := os.Lstat(h.p(claude + "/projects")); err == nil {
		if err := h.agentDir(claude+"/projects", 0o700, u.UID, u.GID); err != nil {
			return fmt.Errorf("home: %w", err)
		}
	}
	for _, name := range c.SeedFiles {
		src, err := os.ReadFile(h.p(initcfg.SeedDir + "/" + name))
		if err != nil {
			return fmt.Errorf("seed file: %w", err)
		}
		if err := h.agentFile(claude+"/"+name, src, u.UID, u.GID, true); err != nil {
			return fmt.Errorf("seed file: %w", err)
		}
	}
	if len(c.ClaudeJSON) > 0 {
		if err := h.agentFile(home+"/.claude.json", c.ClaudeJSON, u.UID, u.GID, false); err != nil {
			return fmt.Errorf("claude.json: %w", err)
		}
	}
	for _, l := range []struct{ name, target string }{{"skills", c.Skills}, {"agents", c.Agents}} {
		if l.target == "" {
			continue
		}
		if err := h.agentSymlink(claude+"/"+l.name, l.target, u.UID, u.GID); err != nil {
			return err
		}
	}
	if len(c.ManagedSettings) > 0 {
		if !json.Valid(c.ManagedSettings) {
			return errors.New("managed settings are not valid JSON")
		}
		dir := path.Dir(initcfg.ManagedSettingsPath)
		if err := os.MkdirAll(h.p(dir), 0o755); err != nil {
			return err
		}
		if err := os.Chmod(h.p(dir), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(h.p(initcfg.ManagedSettingsPath), c.ManagedSettings, 0o644); err != nil {
			return fmt.Errorf("managed settings: %w", err)
		}
	}
	return nil
}

// agentFile writes data to guest with mode 0600, owned by the agent. With
// replace true the data goes into a new file that is renamed over guest, so
// a symlink, hard link, or other file already at guest is replaced and
// whatever it points at or shares is left alone. With replace false an
// existing entry at guest is kept unchanged, and a symlink is never
// followed.
func (h homeFS) agentFile(guest string, data []byte, uid, gid int, replace bool) error {
	if replace {
		if err := writeFileAtomic(h.p(guest), data, 0o600); err != nil {
			return err
		}
		return h.chown(h.p(guest), uid, gid)
	}
	f, err := os.OpenFile(h.p(guest), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return h.chown(h.p(guest), uid, gid)
}

// agentSymlink points guest at target. An existing symlink is replaced;
// anything else at guest is an error.
func (h homeFS) agentSymlink(guest, target string, uid, gid int) error {
	if st, err := os.Lstat(h.p(guest)); err == nil {
		if st.Mode()&fs.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a symlink", guest)
		}
		if err := os.Remove(h.p(guest)); err != nil {
			return err
		}
	}
	if err := os.Symlink(target, h.p(guest)); err != nil {
		return err
	}
	return h.chown(h.p(guest), uid, gid)
}
