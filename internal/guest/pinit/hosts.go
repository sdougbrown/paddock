package pinit

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"strings"

	"github.com/sdougbrown/paddock/internal/egresshost"
)

// hostsPath is the guest's hosts file.
const hostsPath = "/etc/hosts"

// writeHosts rewrites the hosts file at path so that every name in hosts
// maps to addr and to nothing else. Each name must pass egresshost.Validate;
// a bad one fails before the file is touched. Names are written normalized,
// once each. Existing entries are kept, minus the listed names and any
// earlier line for addr. The file is replaced through a rename, root-owned
// with mode 0644.
func writeHosts(path string, hosts []string, addr netip.Addr) error {
	for _, h := range hosts {
		if err := egresshost.Validate(h); err != nil {
			return fmt.Errorf("hosts: %w", err)
		}
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("hosts: %w", err)
	}
	listed := map[string]bool{}
	var names []string
	for _, h := range hosts {
		if n := egresshost.Normalize(h); !listed[n] {
			listed[n] = true
			names = append(names, n)
		}
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(string(old), "\n"), "\n") {
		entry, _, _ := strings.Cut(line, "#")
		f := strings.Fields(entry)
		if len(f) < 2 {
			if line != "" {
				b.WriteString(line + "\n")
			}
			continue
		}
		if a, err := netip.ParseAddr(f[0]); err == nil && a.Unmap() == addr {
			continue
		}
		var kept []string
		for _, n := range f[1:] {
			if !listed[egresshost.Normalize(n)] {
				kept = append(kept, n)
			}
		}
		switch {
		case len(kept) == len(f)-1:
			b.WriteString(line + "\n")
		case len(kept) > 0:
			b.WriteString(f[0] + "\t" + strings.Join(kept, " ") + "\n")
		}
	}
	for _, n := range names {
		fmt.Fprintf(&b, "%s\t%s\n", addr, n)
	}
	if err := writeFileAtomic(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("hosts: %w", err)
	}
	return nil
}
