package pinit

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sdougbrown/paddock/internal/guest/netpolicy"
	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
	"github.com/sdougbrown/paddock/internal/initcfg"
)

// network is the committed policy: the proxy child and the lifeline hosts,
// set for policy allowlist only.
type network struct {
	proxy           *exec.Cmd
	allowed, denied string
}

// stop kills the proxy child, if any, and reaps it when the kill went
// through.
func (n *network) stop() {
	if n != nil && n.proxy != nil && n.proxy.Process.Kill() == nil {
		_ = n.proxy.Wait()
	}
}

// commitNetwork puts cfg's network policy into force with netpolicy.Commit.
// On failure the guest is left at deny-all and the proxy is stopped.
func commitNetwork(ctx context.Context, cfg *initcfg.Config) (*network, error) {
	n := &network{}
	mode := netpolicy.Mode(cfg.Network.Policy)
	if mode != netpolicy.ModeAllowlist {
		if err := netpolicy.Commit(ctx, netpolicy.Policy{Mode: mode}, netpolicy.CommitOptions{}); err != nil {
			return nil, err
		}
		return n, nil
	}
	nc := cfg.Network
	var err error
	if n.allowed, n.denied, err = lifelineHosts(nc.Hosts); err != nil {
		return nil, err
	}
	resolvers, err := netpolicy.ReadResolvConf(netpolicy.ResolvConfPath)
	if err != nil {
		return nil, err
	}
	// Only the proxy uid may query the resolvers, so the agent (and the
	// lifeline, run as root) find allowlisted names here.
	if err := writeHosts(hostsPath, nc.Hosts, netpolicy.HostsAddr); err != nil {
		return nil, err
	}
	addr, err := netpolicy.ProxyAddrFor(resolvers, uint16(nc.ProxyPort))
	if err != nil {
		return nil, err
	}
	errPath := nc.ErrorLog
	if errPath == "" {
		errPath = filepath.Join(filepath.Dir(nc.DenyLog), initcfg.ErrorLogName)
	}
	denyLog, err := openLog(nc.DenyLog)
	if err != nil {
		return nil, err
	}
	defer denyLog.Close()
	errLog, err := openLog(errPath)
	if err != nil {
		return nil, err
	}
	defer errLog.Close()

	policy := netpolicy.Policy{Mode: mode, ProxyUID: cfg.ProxyUID, ProxyAddr: addr, Resolvers: resolvers}
	err = netpolicy.Commit(ctx, policy, netpolicy.CommitOptions{
		StartProxy: func(_ context.Context, p netpolicy.Policy) (int, error) {
			ln, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(p.ProxyAddr))
			if err != nil {
				return 0, err
			}
			defer ln.Close()
			n.proxy, err = sniproxy.Launch(sniproxy.LaunchOptions{
				Path:     initcfg.InitPath,
				Args:     []string{ProxyRole},
				UID:      uint32(p.ProxyUID),
				GID:      uint32(cfg.ProxyGID),
				Listener: ln,
				DenyLog:  denyLog,
				ErrorLog: errLog,
				Config:   sniproxy.ChildConfig{Allow: nc.Hosts, Resolvers: resolvers},
			})
			if err != nil {
				return 0, err
			}
			return n.proxy.Process.Pid, nil
		},
		Lifeline: netpolicy.Lifeline{AllowedHost: n.allowed, DeniedHost: n.denied, Resolvers: resolvers}.Check,
	})
	if err != nil {
		n.stop()
		return nil, err
	}
	return n, nil
}

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}

// lifelineDeniedHost is the server name the lifeline and the verify role
// send to show a name off the allowlist is refused. It is under ".invalid",
// which package egresshost never accepts, so no allowlist can match it; the
// probes dial netpolicy.HostsAddr directly, so it never needs to resolve.
const lifelineDeniedHost = "paddock-lifeline-denied.invalid"

// lifelineHosts validates hosts and picks the lifeline's allowed host
// (api.anthropic.com when listed, else the first host in sorted order) and
// its denied host.
func lifelineHosts(hosts []string) (allowed, denied string, err error) {
	al, err := sniproxy.NewAllowlist(hosts)
	if err != nil {
		return "", "", err
	}
	allowed = al.Hosts()[0]
	if al.Allows("api.anthropic.com") {
		allowed = "api.anthropic.com"
	}
	return allowed, lifelineDeniedHost, nil
}
