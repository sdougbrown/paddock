package netpolicy

import (
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/guest/guesttest"
)

// TestNftAcceptsRulesets checks the generated rulesets with `nft -c`, which
// parses and validates them against the kernel without applying them. It
// needs root and CAP_NET_ADMIN.
func TestNftAcceptsRulesets(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || !guesttest.HasCaps(t, guesttest.CapNetAdmin) {
		t.Skip("needs root with CAP_NET_ADMIN on linux")
	}
	nft, err := NFTPath()
	if err != nil {
		t.Skip(err)
	}
	two := basePolicy()
	two.Resolvers = append(two.Resolvers, netip.MustParseAddr("10.0.0.2"))
	none := basePolicy()
	none.Mode = ModeNone
	for name, p := range map[string]Policy{"allowlist": basePolicy(), "two-resolvers": two, "none": none} {
		t.Run(name, func(t *testing.T) {
			rs, err := Ruleset(p)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(nft, "-c", "-f", "-")
			cmd.Stdin = strings.NewReader(rs)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("nft -c rejected the ruleset: %v\n%s", err, out)
			}
		})
	}
}
