// Package pinit is paddock-init, the guest entrypoint. It runs as root under
// the container's --init, prepares the guest from initcfg.ConfigPath, and
// then becomes the agent:
//
//  1. read the init config, then read and unlink the secrets file;
//  2. create the agent and proxy accounts in /etc/passwd and /etc/group;
//  3. prepare HOME and ~/.claude around the mounts already in place, and
//     write the managed settings;
//  4. commit the network policy (netpolicy.Commit; the SNI proxy runs as a
//     child under the proxy uid);
//  5. drop every capability and set no_new_privs, switch to the agent's
//     uid/gid, and execve the agent's argv with stdio untouched.
//
// With Config.Verify set, step 5 execs paddock-init's verify role as the
// agent instead, which runs the in-guest checks and prints one line each.
//
// Exit status: any failure before the agent is exec'd prints a
// "paddock-init:" line to stderr and exits 125, so it is distinguishable
// from the agent's own exit codes. After the exec the exit status is the
// agent's. The verify role exits 0 when every check passes and 1 otherwise.
package pinit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/sdougbrown/paddock/internal/guest/sniproxy"
	"github.com/sdougbrown/paddock/internal/initcfg"
)

// ExitInit is the exit status for every failure before the agent starts.
const ExitInit = 125

// Roles selected by the first argument. The image's CMD may be appended
// after the entrypoint; any other argument is ignored.
const (
	ProxyRole  = "__sniproxy"
	VerifyRole = "__verify"
)

// Main runs paddock-init with the arguments after argv[0] and returns the
// exit status. On success in the init role it does not return.
func Main(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case ProxyRole:
			err := sniproxy.RunChild()
			fmt.Fprintln(os.Stderr, "paddock-init: proxy:", err)
			return 1
		case VerifyRole:
			return verifyMain(args[1:])
		}
	}
	// Capability bounding set, capset, and no_new_privs are per-thread; the
	// thread that sets them must be the one that calls execve.
	runtime.LockOSThread()
	if err := initMain(); err != nil {
		fmt.Fprintln(os.Stderr, "paddock-init:", err)
		return ExitInit
	}
	return 0
}

func initMain() error {
	cfg, err := initcfg.Read(initcfg.ConfigPath)
	if err != nil {
		return err
	}
	secrets, err := takeSecrets(cfg.Secrets)
	if err != nil {
		return err
	}
	if err := ensureAccounts("/", cfg); err != nil {
		return err
	}
	if err := prepareHome(newHomeFS("/", cfg), cfg); err != nil {
		return err
	}
	env, err := agentEnv(os.Environ(), cfg.User, cfg.Env, secrets)
	if err != nil {
		return err
	}
	net, err := commitNetwork(context.Background(), cfg)
	if err != nil {
		return err
	}
	argv, path, err := agentCommand(cfg, net, env)
	if err != nil {
		net.stop()
		return err
	}
	// After the drop the agent uid cannot signal the proxy, so a failure
	// from here on leaves it to end with the container.
	return dropAndExec(cfg.User.UID, cfg.User.GID, cfg.Workdir, path, argv, env)
}

// agentCommand returns the argv to exec and its resolved path, checking
// while init can still stop the proxy that the command and the workdir
// exist.
func agentCommand(cfg *initcfg.Config, net *network, env []string) ([]string, string, error) {
	argv := cfg.Argv
	if cfg.Verify {
		var err error
		if argv, err = verifyArgv(cfg, net); err != nil {
			return nil, "", err
		}
	}
	if st, err := os.Stat(cfg.Workdir); err != nil || !st.IsDir() {
		return nil, "", fmt.Errorf("workdir %s is not a directory (%v)", cfg.Workdir, err)
	}
	path, err := lookPath(argv[0], envValue(env, "PATH"))
	return argv, path, err
}

// verifyArgv is the command line of the verify role for cfg.
func verifyArgv(cfg *initcfg.Config, net *network) ([]string, error) {
	p := verifyPlan{
		Policy:   cfg.Network.Policy,
		UID:      cfg.User.UID,
		DenyLog:  cfg.Network.DenyLog,
		Writable: []string{cfg.User.Home, cfg.User.Home + "/.claude"},
	}
	if net.proxy != nil {
		p.ProxyPID = net.proxy.Process.Pid
		p.AllowedHost, p.DeniedHost = net.allowed, net.denied
	}
	if t := cfg.Claude.TranscriptDir; t != "" {
		p.Writable = append(p.Writable, t)
	}
	for _, m := range cfg.Mounts {
		if m.ReadOnly {
			p.ReadOnly = append(p.ReadOnly, m.Path)
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return []string{initcfg.InitPath, VerifyRole, string(b)}, nil
}
