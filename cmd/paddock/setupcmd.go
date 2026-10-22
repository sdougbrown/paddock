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
	o := resolve.Options{Phase: resolve.PhaseSetup}
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
	hit, release, err := resolve.PrepareSetupCache(p.Setup)
	if err != nil {
		return 1, err
	}
	defer release()
	eng, err := newEngine(p.Engine)
	if err != nil {
		return 1, err
	}
	start := time.Now()
	code, err := eng.Run(context.Background(), p, engine.Options{})
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
