package pinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sdougbrown/paddock/internal/envname"
	"github.com/sdougbrown/paddock/internal/initcfg"
)

// DefaultPath is the agent's PATH unless the inherited environment or
// Config.Env sets one.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// takeSecrets reads the secrets file at path and unlinks it. An empty path
// means there are no secrets. The file is unlinked even when it does not
// parse. Errors never include a value.
func takeSecrets(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	data, rerr := os.ReadFile(path)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("secrets: unlink %s: %w", path, err)
	}
	if rerr != nil {
		return nil, fmt.Errorf("secrets: %w", rerr)
	}
	return parseSecrets(data)
}

// parseSecrets parses KEY=VALUE lines. Blank lines are skipped. Errors name
// the line number and, when it is valid, the key; never the value.
func parseSecrets(data []byte) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || !envname.Valid(name) {
			return nil, fmt.Errorf("secrets line %d: not NAME=VALUE", i+1)
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("secrets line %d: %s: value contains a NUL byte", i+1, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("secrets line %d: %s is set twice", i+1, name)
		}
		seen[name] = true
		out = append(out, name+"="+value)
	}
	return out, nil
}

// agentEnv is the agent's environment. It starts from inherited (the init
// process's environment), then sets PATH (DefaultPath only when inherited
// has none), HOME, USER, LOGNAME, and SHELL, then adds extra (Config.Env),
// then secrets. A later entry replaces an earlier one with the same name,
// in the earlier one's position. An invalid extra or secret entry is quoted
// in the error, so secrets must be validated by parseSecrets first.
func agentEnv(inherited []string, u initcfg.User, extra, secrets []string) ([]string, error) {
	var env []string
	index := map[string]int{}
	set := func(kv string) {
		name, _, _ := strings.Cut(kv, "=")
		if i, ok := index[name]; ok {
			env[i] = kv
			return
		}
		index[name] = len(env)
		env = append(env, kv)
	}
	for _, kv := range inherited {
		set(kv)
	}
	if _, ok := index["PATH"]; !ok {
		set("PATH=" + DefaultPath)
	}
	set("HOME=" + u.Home)
	set("USER=" + u.Name)
	set("LOGNAME=" + u.Name)
	set("SHELL=" + agentShell)
	for _, kv := range append(append([]string(nil), extra...), secrets...) {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !envname.Valid(name) {
			return nil, fmt.Errorf("env entry %q is not NAME=VALUE", name)
		}
		set(kv)
	}
	return env, nil
}

// lookPath finds file on the colon-separated list dirs, the way execvp
// does: a name containing a slash is used as is.
func lookPath(file, dirs string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	for _, dir := range filepath.SplitList(dirs) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, file)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found on PATH %s", file, dirs)
}

// envValue returns the value of name in env.
func envValue(env []string, name string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}
