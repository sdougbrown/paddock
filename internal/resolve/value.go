package resolve

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sdougbrown/paddock/internal/plan"
)

// ParseSource classifies a raw env value: "file:<path>", "env:<NAME>", or a
// literal. Refs of file and env sources are returned unexpanded.
func ParseSource(raw string) (kind, ref string, err error) {
	switch {
	case strings.HasPrefix(raw, "file:"):
		ref = strings.TrimPrefix(raw, "file:")
		if ref == "" {
			return "", "", errors.New("file: source has no path")
		}
		return plan.SourceFile, ref, nil
	case strings.HasPrefix(raw, "env:"):
		ref = strings.TrimPrefix(raw, "env:")
		if ref == "" {
			return "", "", errors.New("env: source has no variable name")
		}
		return plan.SourceEnv, ref, nil
	}
	return plan.SourceLiteral, raw, nil
}

// ExpandHome expands a leading "~" to home and requires the result to be an
// absolute path.
func ExpandHome(p, home string) (string, error) {
	switch {
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be absolute (or start with ~/)", p)
	}
	return filepath.Clean(p), nil
}

// ResolveValue reads the value behind an EnvVar at launch time. A file value
// is trimmed of surrounding whitespace and must contain no interior
// whitespace or control characters; an env value must be set and non-empty.
// Errors never include the value.
func ResolveValue(v plan.EnvVar, lookup func(string) (string, bool)) (string, error) {
	switch v.Source {
	case plan.SourceLiteral:
		return v.Value, nil
	case plan.SourceEnv:
		val, ok := lookup(v.Ref)
		if !ok || val == "" {
			return "", fmt.Errorf("env.%s: environment variable %s is unset or empty", v.Name, v.Ref)
		}
		return val, nil
	case plan.SourceFile:
		data, err := os.ReadFile(v.Ref)
		if err != nil {
			return "", fmt.Errorf("env.%s: %w", v.Name, err)
		}
		val := strings.TrimSpace(string(data))
		if val == "" {
			return "", fmt.Errorf("env.%s: %s is empty", v.Name, v.Ref)
		}
		for _, r := range val {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return "", fmt.Errorf("env.%s: %s contains whitespace or control characters inside the value", v.Name, v.Ref)
			}
		}
		return val, nil
	}
	return "", fmt.Errorf("env.%s: unknown source %q", v.Name, v.Source)
}
