package sniproxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// The proxy runs as a separate process under its own uid so the agent can
// neither signal it nor share its exemption from the tcp/443 DNAT. The
// parent (root) binds the listener and opens the log files, so the proxy
// uid needs no filesystem or bind permissions. Child file descriptors:
//
//	stdin   ChildConfig as JSON
//	stdout  the child writes "ready\n" once serving, then closes it
//	stderr  the error log
//	fd 3    the listening socket
//	fd 4    the denial log (append)
//	fd 5    the error log
const (
	childListenerFD = 3
	childDenyLogFD  = 4
	childErrorLogFD = 5
	readyLine       = "ready"
)

// ChildConfig is what the parent sends the proxy child on stdin.
type ChildConfig struct {
	Allow []string `json:"allow"`
	// Resolvers are the IPv4 DNS servers the child queries on port 53,
	// through a DNSResolver. The proxy never dials them on the upstream
	// port.
	Resolvers []netip.Addr `json:"resolvers"`
}

// UsableResolver reports whether a can be queried as a DNS server: an IPv4
// address (possibly v4-mapped) that is not the unspecified address.
func UsableResolver(a netip.Addr) bool {
	a = a.Unmap()
	return a.Is4() && !a.IsUnspecified()
}

// resolver returns the child's DNSResolver.
func (c ChildConfig) resolver() (*DNSResolver, error) {
	if len(c.Resolvers) == 0 {
		return nil, errors.New("sniproxy: child config lists no resolvers")
	}
	r := &DNSResolver{}
	for _, a := range c.Resolvers {
		if !UsableResolver(a) {
			return nil, fmt.Errorf("sniproxy: resolver address %q is not a usable IPv4 address", a)
		}
		r.Servers = append(r.Servers, netip.AddrPortFrom(a.Unmap(), 53))
	}
	return r, nil
}

// LaunchOptions describes how to start the proxy child.
type LaunchOptions struct {
	// Path and Args are the command to exec; it must call RunChild.
	// Args excludes argv[0], which is set to Path.
	Path string
	Args []string
	// UID and GID the child runs as; neither may be 0. Supplementary
	// groups are cleared.
	UID, GID uint32
	// Listener is the bound listening socket the child serves.
	Listener *net.TCPListener
	// DenyLog and ErrorLog are open for appending. A nil ErrorLog
	// discards operational messages.
	DenyLog  *os.File
	ErrorLog *os.File
	Config   ChildConfig
	// ReadyTimeout bounds the wait for the child's ready line; default 5s.
	ReadyTimeout time.Duration
}

// Launch starts the proxy child as UID/GID in a new session and waits for
// it to report ready. It returns the running command; the caller owns
// waiting for it. The caller may close its copy of Listener afterwards.
func Launch(o LaunchOptions) (*exec.Cmd, error) {
	if _, err := NewAllowlist(o.Config.Allow); err != nil {
		return nil, err
	}
	if _, err := o.Config.resolver(); err != nil {
		return nil, err
	}
	if o.Listener == nil || o.DenyLog == nil {
		return nil, errors.New("sniproxy: launch needs a listener and a denial log")
	}
	if o.UID == 0 {
		return nil, errors.New("sniproxy: refusing to run the proxy as root")
	}
	if o.GID == 0 {
		return nil, errors.New("sniproxy: refusing to run the proxy in group root")
	}
	lf, err := o.Listener.File()
	if err != nil {
		return nil, fmt.Errorf("sniproxy: listener fd: %w", err)
	}
	defer lf.Close()
	errLog := o.ErrorLog
	if errLog == nil {
		devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		defer devnull.Close()
		errLog = devnull
	}
	cfg, err := json.Marshal(o.Config)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(o.Path, o.Args...)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.Stdin = strings.NewReader(string(cfg))
	cmd.Stderr = errLog
	cmd.ExtraFiles = []*os.File{lf, o.DenyLog, errLog}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true,
		Credential: &syscall.Credential{Uid: o.UID, Gid: o.GID, Groups: []uint32{}},
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sniproxy: start proxy: %w", err)
	}

	timeout := o.ReadyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if strings.TrimSpace(line) == readyLine {
			ready <- nil
			return
		}
		if err == nil {
			err = fmt.Errorf("unexpected output %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err == nil {
			return cmd, nil
		}
		_ = cmd.Process.Kill()
		werr := cmd.Wait()
		return nil, fmt.Errorf("sniproxy: proxy did not become ready: %v (exit: %v)", err, werr)
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("sniproxy: proxy not ready after %v", timeout)
	}
}

// RunChild is the proxy child's entry point (see Launch). It serves until
// the listener fails and returns only on error.
func RunChild() error {
	var cfg ChildConfig
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&cfg); err != nil {
		return fmt.Errorf("sniproxy: read config: %w", err)
	}
	allow, err := NewAllowlist(cfg.Allow)
	if err != nil {
		return err
	}
	resolver, err := cfg.resolver()
	if err != nil {
		return err
	}
	lf := os.NewFile(childListenerFD, "listener")
	ln, err := net.FileListener(lf)
	if err != nil {
		return fmt.Errorf("sniproxy: listener on fd %d: %w", childListenerFD, err)
	}
	lf.Close()
	denyLog := os.NewFile(childDenyLogFD, "deny-log")
	errLog := log.New(os.NewFile(childErrorLogFD, "error-log"), "sniproxy: ", log.LstdFlags|log.LUTC|log.Lmicroseconds)
	p, err := New(Config{Allowlist: allow, Resolver: resolver, ExcludeAddrs: cfg.Resolvers, DenyLog: denyLog, ErrorLog: errLog})
	if err != nil {
		return err
	}
	errLog.Printf("serving %s as uid %d for %d patterns", ln.Addr(), os.Getuid(), len(cfg.Allow))
	if _, err := fmt.Fprintln(os.Stdout, readyLine); err != nil {
		return err
	}
	os.Stdout.Close()
	if err := p.Serve(ln); err != nil {
		return err
	}
	return errors.New("sniproxy: listener closed")
}
