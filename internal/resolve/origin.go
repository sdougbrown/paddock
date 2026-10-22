package resolve

import (
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/sdougbrown/paddock/internal/config"
)

// ParseOrigin extracts "<owner>/<repo>" from a git remote URL: https, http,
// ssh://, git://, or scp-style (user@host:owner/repo.git).
func ParseOrigin(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	var path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", false
		}
		path = u.Path
	} else if i := strings.Index(raw, ":"); i > 0 && !strings.Contains(raw[:i], "/") {
		path = raw[i+1:]
	} else {
		return "", false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return "", false
	}
	ref := parts[0] + "/" + parts[1]
	if !config.ValidRef(ref) {
		return "", false
	}
	return ref, true
}

// redactOrigin returns a form of a git remote URL that is safe to print: URL
// forms lose their userinfo, and scp-style remotes print only the parsed
// owner/repo.
func redactOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "(unparseable URL)"
		}
		u.User = nil
		return u.String()
	}
	if i := strings.Index(raw, ":"); i > 0 && !strings.Contains(raw[:i], "/") {
		if ref, ok := ParseOrigin(raw); ok {
			return ref
		}
		return "(scp-style remote)"
	}
	return raw
}

// GitOrigin returns the origin remote URL of the repository containing dir.
func GitOrigin(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
