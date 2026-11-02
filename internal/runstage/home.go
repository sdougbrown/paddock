package runstage

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

// HomeFS is the filesystem PrepareHome works on. Guest paths are joined onto
// root; chown is os.Lchown in the guest; mounts lists guest mount points,
// which are never created, chowned, or replaced. Both the container engines
// (through paddock-init) and the bwrap engine materialize the agent HOME
// through it, so the layout has one owner.
type HomeFS struct {
	Root   string
	Chown  func(path string, uid, gid int) error
	Mounts map[string]bool
	// SeedDir is where the config's named seed files are read from. The
	// empty value means the init config's guest path, which paddock-init
	// finds under its state mount; the bwrap engine points it at the run's
	// staged seed directory on the host.
	SeedDir string
}

// NewHomeFS builds the filesystem view for an init config: the guest root,
// ownership via Lchown, and the config's mounts and transcript directory as
// never-replace mount points.
func NewHomeFS(root string, cfg *initcfg.Config) HomeFS {
	h := HomeFS{Root: root, Chown: os.Lchown, Mounts: map[string]bool{}}
	for _, m := range cfg.Mounts {
		h.Mounts[path.Clean(m.Path)] = true
	}
	if t := cfg.Claude.TranscriptDir; t != "" {
		h.Mounts[path.Clean(t)] = true
	}
	return h
}

func (h HomeFS) P(guest string) string { return filepath.Join(h.Root, guest) }

// IsMount reports whether guest is a listed mount point or sits on a
// different device from its parent.
func (h HomeFS) IsMount(guest string) bool {
	if h.Mounts[guest] {
		return true
	}
	st, err1 := os.Lstat(h.P(guest))
	pst, err2 := os.Lstat(h.P(path.Dir(guest)))
	if err1 != nil || err2 != nil {
		return false
	}
	a, ok1 := st.Sys().(*syscall.Stat_t)
	b, ok2 := pst.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && a.Dev != b.Dev
}

// AgentDir makes guest exist as a directory owned by the agent, and refuses
// the filesystem root. An existing mount point is left alone. Missing
// parents are created root-owned 0755.
func (h HomeFS) AgentDir(guest string, mode os.FileMode, uid, gid int) error {
	if path.Clean(guest) == "/" {
		return errors.New("refusing to take ownership of the filesystem root")
	}
	if h.IsMount(guest) {
		return nil
	}
	if err := os.MkdirAll(h.P(path.Dir(guest)), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(h.P(guest), mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	st, err := os.Lstat(h.P(guest))
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", guest)
	}
	return h.Chown(h.P(guest), uid, gid)
}

// AgentDirs lists the directories under home that the agent owns and may
// write: home itself and ~/.claude.
func AgentDirs(home string) []DirSpec {
	return []DirSpec{{Path: home, Mode: 0o755}, {Path: home + "/.claude", Mode: 0o700}}
}

// DirSpec is a directory the agent owns.
type DirSpec struct {
	Path string
	Mode os.FileMode
}

// PrepareHome lays out the agent's HOME: HOME and ~/.claude (and
// ~/.claude/projects when it already exists) owned by the agent, seed files
// and ~/.claude.json, the skills and agents symlinks, and the managed
// settings file. When ~/.claude/projects does not exist and the init config
// carries a transcript directory, projects becomes a symlink to it, so
// transcript writes land on the host path the caller derives.
func PrepareHome(h HomeFS, cfg *initcfg.Config) error {
	u, c := cfg.User, cfg.Claude
	home := path.Clean(u.Home)
	claude := home + "/.claude"
	for _, d := range AgentDirs(home) {
		if err := h.AgentDir(d.Path, d.Mode, u.UID, u.GID); err != nil {
			return fmt.Errorf("home: %w", err)
		}
	}
	if _, err := os.Lstat(h.P(claude + "/projects")); err == nil {
		if err := h.AgentDir(claude+"/projects", 0o700, u.UID, u.GID); err != nil {
			return fmt.Errorf("home: %w", err)
		}
	} else if c.TranscriptDir != "" {
		if err := h.AgentSymlink(claude+"/projects", c.TranscriptDir, u.UID, u.GID); err != nil {
			return fmt.Errorf("transcript dir: %w", err)
		}
	}
	seedDir := h.SeedDir
	if seedDir == "" {
		seedDir = initcfg.SeedDir
	}
	for _, name := range c.SeedFiles {
		src, err := os.ReadFile(h.P(seedDir + "/" + name))
		if err != nil {
			return fmt.Errorf("seed file: %w", err)
		}
		if err := h.AgentFile(claude+"/"+name, src, u.UID, u.GID, true); err != nil {
			return fmt.Errorf("seed file: %w", err)
		}
	}
	if len(c.ClaudeJSON) > 0 {
		if err := h.AgentFile(home+"/.claude.json", c.ClaudeJSON, u.UID, u.GID, false); err != nil {
			return fmt.Errorf("claude.json: %w", err)
		}
	}
	for _, l := range []struct{ name, target string }{{"skills", c.Skills}, {"agents", c.Agents}} {
		if l.target == "" {
			continue
		}
		if err := h.AgentSymlink(claude+"/"+l.name, l.target, u.UID, u.GID); err != nil {
			return err
		}
	}
	if len(c.ManagedSettings) > 0 {
		if !json.Valid(c.ManagedSettings) {
			return errors.New("managed settings are not valid JSON")
		}
		dir := path.Dir(initcfg.ManagedSettingsPath)
		if err := os.MkdirAll(h.P(dir), 0o755); err != nil {
			return err
		}
		if err := os.Chmod(h.P(dir), 0o755); err != nil {
			return err
		}
		if err := WriteFileAtomic(h.P(initcfg.ManagedSettingsPath), c.ManagedSettings, 0o644); err != nil {
			return fmt.Errorf("managed settings: %w", err)
		}
	}
	return nil
}

// AgentFile writes data to guest with mode 0600, owned by the agent. With
// replace true the data goes into a new file that is renamed over guest, so
// a symlink, hard link, or other file already at guest is replaced and
// whatever it points at or shares is left alone. With replace false an
// existing entry at guest is kept unchanged, and a symlink is never
// followed.
func (h HomeFS) AgentFile(guest string, data []byte, uid, gid int, replace bool) error {
	if replace {
		if err := WriteFileAtomic(h.P(guest), data, 0o600); err != nil {
			return err
		}
		return h.Chown(h.P(guest), uid, gid)
	}
	f, err := os.OpenFile(h.P(guest), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
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
	return h.Chown(h.P(guest), uid, gid)
}

// AgentSymlink points guest at target. An existing symlink is replaced;
// anything else at guest is an error.
func (h HomeFS) AgentSymlink(guest, target string, uid, gid int) error {
	if st, err := os.Lstat(h.P(guest)); err == nil {
		if st.Mode()&fs.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a symlink", guest)
		}
		if err := os.Remove(h.P(guest)); err != nil {
			return err
		}
	}
	if err := os.Symlink(target, h.P(guest)); err != nil {
		return err
	}
	return h.Chown(h.P(guest), uid, gid)
}

func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EBUSY) {
		return err
	}
	return writeFileInPlace(path, data, mode)
}

// writeFileInPlace rewrites path with data, keeping its inode and setting
// mode. WriteFileAtomic falls back to it when a bind mount pins the file,
// as podman's /etc/hosts does: rename can never replace that inode. The
// caller runs as root before the privilege drop.
func writeFileInPlace(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
