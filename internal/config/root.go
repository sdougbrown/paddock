// Package config holds the strict TOML types for profiles and specs and the
// loaders that read them from a config root.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Root is a paddock config directory.
type Root struct{ Dir string }

// ResolveRoot picks the config root: $PADDOCK_CONFIG_DIR, else
// $XDG_CONFIG_HOME/paddock, else <home>/.config/paddock.
func ResolveRoot(getenv func(string) string, home string) (Root, error) {
	if d := getenv("PADDOCK_CONFIG_DIR"); d != "" {
		return Root{Dir: d}, nil
	}
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		return Root{Dir: filepath.Join(d, "paddock")}, nil
	}
	if home == "" {
		return Root{}, errors.New("config root: no PADDOCK_CONFIG_DIR, XDG_CONFIG_HOME, or home directory")
	}
	return Root{Dir: filepath.Join(home, ".config", "paddock")}, nil
}

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	envRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidRef reports whether ref is a spec reference: "<owner>" or "<owner>/<repo>".
func ValidRef(ref string) bool {
	parts := strings.Split(ref, "/")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !nameRe.MatchString(p) || strings.Contains(p, "..") {
			return false
		}
	}
	return true
}

// Path joins a root-relative path onto the root, rejecting absolute paths and
// paths that escape the root.
func (r Root) Path(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative to the config root", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the config root", rel)
	}
	return filepath.Join(r.Dir, clean), nil
}

// LoadProfile reads and validates profiles/<name>.toml.
func (r Root) LoadProfile(name string) (*Profile, error) {
	if !ValidRef(name) || strings.Contains(name, "/") {
		return nil, fmt.Errorf("invalid profile name %q", name)
	}
	var p Profile
	if err := r.load(filepath.Join(r.Dir, "profiles", name+".toml"), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SpecPath is the file a spec ref loads from.
func (r Root) SpecPath(ref string) string {
	return filepath.Join(r.Dir, "projects", filepath.FromSlash(ref)+".toml")
}

// LoadSpec reads and validates projects/<ref>.toml. A missing file wraps
// os.ErrNotExist.
func (r Root) LoadSpec(ref string) (*Spec, error) {
	if !ValidRef(ref) {
		return nil, fmt.Errorf("invalid spec ref %q", ref)
	}
	var s Spec
	if err := r.load(r.SpecPath(ref), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

type validator interface{ validate() error }

func (r Root) load(path string, v validator) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decode(path, data, v)
}

// decode strictly decodes data into v and validates it. Errors are prefixed
// with path; decode errors add line:column and the key, and unknown keys name
// each key with its position.
func decode(path string, data []byte, v validator) error {
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			msgs := make([]string, 0, len(sm.Errors))
			for i := range sm.Errors {
				msgs = append(msgs, locate(path, &sm.Errors[i], "unknown key"))
			}
			return errors.New(strings.Join(msgs, "\n"))
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			return errors.New(locate(path, de, de.Error()))
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := v.validate(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// locate formats a decode error as <path>:<line>:<col>: <key>: <message>,
// omitting the key when the error carries none.
func locate(path string, de *toml.DecodeError, msg string) string {
	row, col := de.Position()
	out := fmt.Sprintf("%s:%d:%d: ", path, row, col)
	if key := strings.Join(de.Key(), "."); key != "" {
		out += key + ": "
	}
	return out + msg
}

func validEnvName(n string) bool { return envRe.MatchString(n) }
