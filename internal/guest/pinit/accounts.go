package pinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sdougbrown/paddock/internal/initcfg"
)

// Proxy account names and the agent's login shell.
const (
	proxyName  = "paddock-proxy"
	agentShell = "/bin/bash"
)

// account is one /etc/passwd entry and its primary group.
type account struct {
	name, group string
	uid, gid    int
	home, shell string
}

// ensureAccounts adds the agent and, when a proxy uid is set, the proxy
// account under root.
func ensureAccounts(root string, cfg *initcfg.Config) error {
	u := cfg.User
	if err := ensureAccount(root, account{u.Name, u.Name, u.UID, u.GID, u.Home, agentShell}); err != nil {
		return err
	}
	if cfg.ProxyUID <= 0 {
		return nil
	}
	if cfg.ProxyGID <= 0 {
		return fmt.Errorf("invalid proxy gid %d", cfg.ProxyGID)
	}
	return ensureAccount(root, account{proxyName, proxyName, cfg.ProxyUID, cfg.ProxyGID, "/nonexistent", "/usr/sbin/nologin"})
}

// ensureAccount makes a's group and user exist. A group that already has
// a.gid is reused whatever its name. A user that already exists with a.uid
// and a.name is kept when its gid, home, and shell match; a.uid held by
// another name, a.name held by another uid, or an existing entry with a
// different gid, home, or shell is an error.
func ensureAccount(root string, a account) error {
	for _, f := range []string{a.name, a.group, a.home, a.shell} {
		if f == "" || strings.ContainsAny(f, ":\n") {
			return fmt.Errorf("account %q: invalid passwd field %q", a.name, f)
		}
	}
	groupPath := filepath.Join(root, "etc/group")
	groups, err := readDB(groupPath)
	if err != nil {
		return err
	}
	if g := findEntry(groups, 2, a.gid); g == nil {
		if g := findName(groups, a.group); g != nil {
			return fmt.Errorf("group %q exists with gid %s, want %d", a.group, g[2], a.gid)
		}
		line := fmt.Sprintf("%s:x:%d:", a.group, a.gid)
		if err := appendDB(groupPath, groups, line); err != nil {
			return err
		}
	}

	passwdPath := filepath.Join(root, "etc/passwd")
	users, err := readDB(passwdPath)
	if err != nil {
		return err
	}
	if u := findEntry(users, 2, a.uid); u != nil {
		if u[0] != a.name {
			return fmt.Errorf("uid %d is taken by user %q, want %q", a.uid, u[0], a.name)
		}
		if len(u) < 7 || u[3] != strconv.Itoa(a.gid) || u[5] != a.home || u[6] != a.shell {
			return fmt.Errorf("user %q already exists as %q, want gid %d, home %s, shell %s",
				a.name, strings.Join(u, ":"), a.gid, a.home, a.shell)
		}
		return nil
	}
	if u := findName(users, a.name); u != nil {
		return fmt.Errorf("user %q exists with uid %s, want %d", a.name, u[2], a.uid)
	}
	line := fmt.Sprintf("%s:x:%d:%d::%s:%s", a.name, a.uid, a.gid, a.home, a.shell)
	return appendDB(passwdPath, users, line)
}

// readDB reads a colon-separated database as lines. A missing file is
// empty.
func readDB(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n"), nil
}

// findEntry returns the fields of the first line whose field i is id.
func findEntry(lines []string, i, id int) []string {
	want := strconv.Itoa(id)
	for _, l := range lines {
		if f := strings.Split(l, ":"); len(f) > i && f[i] == want {
			return f
		}
	}
	return nil
}

func findName(lines []string, name string) []string {
	for _, l := range lines {
		if f := strings.Split(l, ":"); len(f) > 2 && f[0] == name {
			return f
		}
	}
	return nil
}

// appendDB rewrites path as lines plus line, through a temporary file and a
// rename, with mode 0644.
func appendDB(path string, lines []string, line string) error {
	var b strings.Builder
	for _, l := range lines {
		if l != "" {
			b.WriteString(l + "\n")
		}
	}
	b.WriteString(line + "\n")
	return writeFileAtomic(path, []byte(b.String()), 0o644)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
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
	return os.Rename(tmp.Name(), path)
}
