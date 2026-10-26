package pinit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/sdougbrown/paddock/internal/guest/verify"
	"github.com/sdougbrown/paddock/internal/initcfg"
)

// verifyPlan is the verify role's single argument, as JSON.
type verifyPlan struct {
	Policy      string   `json:"policy"`
	UID         int      `json:"uid"`
	AllowedHost string   `json:"allowed_host,omitempty"`
	DeniedHost  string   `json:"denied_host,omitempty"`
	DenyLog     string   `json:"deny_log,omitempty"`
	ProxyPID    int      `json:"proxy_pid,omitempty"`
	ReadOnly    []string `json:"read_only,omitempty"`
	Writable    []string `json:"writable,omitempty"`
}

// verifyMain runs as the agent, exec'd by the init role after the privilege
// drop, and prints one line per check.
func verifyMain(args []string) int {
	var p verifyPlan
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "paddock-init: verify: want one JSON argument")
		return ExitInit
	}
	if err := json.Unmarshal([]byte(args[0]), &p); err != nil {
		fmt.Fprintln(os.Stderr, "paddock-init: verify:", err)
		return ExitInit
	}
	checks := networkChecks(p)
	checks = append(checks, verify.LocalChecks(verify.LocalOptions{
		UID:      p.UID,
		ReadOnly: p.ReadOnly,
		Writable: p.Writable,
	})...)
	if !verify.Write(os.Stdout, verify.Run(context.Background(), checks)) {
		return 1
	}
	return 0
}

// networkChecks returns the network checks for p's policy. The host policy
// has no guest network rules to probe, and an unknown policy fails.
func networkChecks(p verifyPlan) []verify.Check {
	switch p.Policy {
	case initcfg.PolicyAllowlist:
		return verify.NetworkChecks(verify.NetworkOptions{
			AllowedHost: p.AllowedHost,
			DeniedHost:  p.DeniedHost,
			DenyLog:     p.DenyLog,
			ProxyPID:    p.ProxyPID,
		})
	case initcfg.PolicyNone:
		return verify.ClosedNetworkChecks(verify.NetworkOptions{})
	case initcfg.PolicyHost:
		return []verify.Check{{Name: "network", Run: func(context.Context) (string, error) {
			return "policy host, no network checks apply", nil
		}}}
	default:
		return []verify.Check{{Name: "network", Run: func(context.Context) (string, error) {
			return "", fmt.Errorf("unknown network policy %q", p.Policy)
		}}}
	}
}
