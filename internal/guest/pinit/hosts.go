package pinit

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"strings"
)

// hostsPath is the guest's hosts file.
const hostsPath = "/etc/hosts"

// writeHosts rewrites the hosts file at path so that every name in hosts
// maps to addr and to nothing else. Existing entries are kept, minus the
// listed names and any earlier line for addr. The file is replaced through
// a rename, root-owned with mode 0644.
func writeHosts(path string, hosts []string, addr netip.Addr) error {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("hosts: %w", err)
	}
	listed := map[string]bool{}
	for _, h := range hosts {
		listed[hostsKey(h)] = true
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
		var names []string
		for _, n := range f[1:] {
			if !listed[hostsKey(n)] {
				names = append(names, n)
			}
		}
		switch {
		case len(names) == len(f)-1:
			b.WriteString(line + "\n")
		case len(names) > 0:
			b.WriteString(f[0] + "\t" + strings.Join(names, " ") + "\n")
		}
	}
	for _, h := range hosts {
		fmt.Fprintf(&b, "%s\t%s\n", addr, h)
	}
	if err := writeFileAtomic(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("hosts: %w", err)
	}
	return nil
}

func hostsKey(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".")
}
