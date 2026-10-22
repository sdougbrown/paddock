// Package envname defines the environment variable names paddock accepts. It
// is a leaf package shared by config loading and paddock-init, so both apply
// the same rule.
package envname

// Valid reports whether s is a POSIX environment variable name: a letter or
// underscore followed by letters, digits, or underscores.
func Valid(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
