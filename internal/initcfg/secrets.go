package initcfg

import (
	"fmt"
	"strings"

	"github.com/sdougbrown/paddock/internal/envname"
)

// ParseSecrets parses the secrets.env format: KEY=VALUE lines. Blank lines
// are skipped and carriage returns stripped. Errors name the line number
// and, when it is valid, the key; never the value — a value may contain
// anything, and it must never reach an error message.
func ParseSecrets(data []byte) ([]string, error) {
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
