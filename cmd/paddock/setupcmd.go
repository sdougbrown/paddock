package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/plan"
	"github.com/sdougbrown/paddock/internal/resolve"
)

// setupCmd runs the spec's setup command in a paddock with the dependency
// cache mounted, prints one summary line to stderr, and returns the
// command's exit code.
func setupCmd(args []string, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := resolve.Options{Phase: plan.PhaseSetup}
	fs.StringVar(&o.Profile, "profile", "", "profile name (required)")
	fs.StringVar(&o.Spec, "spec", "", "spec ref (default: from git origin, else profile default_spec)")
	fs.StringVar(&o.Workspace, "workspace", "", "workspace directory (default: current directory)")
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	if o.Profile == "" {
		return 1, fmt.Errorf("--profile is required")
	}
	if fs.NArg() > 0 {
		return 1, fmt.Errorf("the setup command comes from the spec; unexpected arguments %q", fs.Args())
	}
	home, _ := os.UserHomeDir()
	root, err := config.ResolveRoot(os.Getenv, home)
	if err != nil {
		return 1, err
	}
	p, err := resolve.New(root, home).Resolve(o)
	if err != nil {
		return 1, err
	}
	eng, err := newEngine(p.Engine)
	if err != nil {
		return 1, err
	}
	return runSetup(context.Background(), eng, p, stderr)
}

// runSetup runs a setup-phase plan holding its cache lock, marks the cache
// complete only when the command exits 0, and prints the summary line.
//
// The lock belongs to this process. If paddock is killed, the lock is
// released while the engine's watchdog is still stopping the VM, so a setup
// started in those few seconds can write the same cache as the dying one.
// The dying run never marks the cache complete.
func runSetup(ctx context.Context, eng engine.Engine, p *plan.Plan, stderr io.Writer) (int, error) {
	hit, release, err := resolve.PrepareSetupCache(p.Setup)
	if err != nil {
		return 1, err
	}
	defer release()
	start := time.Now()
	code, err := eng.Run(ctx, p, engine.Options{})
	if err == nil && code == 0 {
		if err = resolve.CompleteSetupCache(p.Setup); err != nil {
			code = 1
		}
	}
	fmt.Fprintln(stderr, summaryLine(p.Setup, hit, time.Since(start), code))
	return code, err
}

func summaryLine(s *plan.SetupPlan, hit bool, d time.Duration, code int) string {
	if s.CacheHost == "" {
		return fmt.Sprintf("paddock setup: no cache, %s, exit %d", d.Round(time.Millisecond), code)
	}
	state := "miss"
	if hit {
		state = "hit"
	}
	return fmt.Sprintf("paddock setup: cache %s, %s, %s, exit %d", state, s.CacheHost, d.Round(time.Millisecond), code)
}
