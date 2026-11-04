package pinit

import (
	"github.com/sdougbrown/paddock/internal/initcfg"

	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

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

// parseSecrets parses the shared secrets.env format; initcfg owns it.
func parseSecrets(data []byte) ([]string, error) {
	return initcfg.ParseSecrets(data)
}

// lookPath finds file on the colon-separated list dirs: a name containing a
// slash is used as is. Unlike execvp, an empty entry is skipped rather than
// read as the current directory, so a file in the agent's working directory
// never stands in for a command on PATH.
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
