package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/engine/applecontainer"
	"github.com/sdougbrown/paddock/internal/resolve"
)

// runCmd resolves a plan and runs it, returning the command's exit code.
func runCmd(args []string, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o resolve.Options
	var verify bool
	fs.StringVar(&o.Profile, "profile", "", "profile name (required)")
	fs.StringVar(&o.Spec, "spec", "", "spec ref (default: from git origin, else profile default_spec)")
	fs.StringVar(&o.Workspace, "workspace", "", "workspace directory (default: current directory)")
	fs.BoolVar(&verify, "verify", false, "run the in-guest isolation checks instead of a command")
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	if o.Profile == "" {
		return 1, fmt.Errorf("--profile is required")
	}
	o.Argv = fs.Args()
	if len(o.Argv) == 0 && !verify {
		return 1, fmt.Errorf("a command is required after --")
	}
	if len(o.Argv) > 0 && verify {
		return 1, fmt.Errorf("--verify takes no command")
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
	return eng.Run(context.Background(), p, engine.Options{Verify: verify})
}

func newEngine(name string) (engine.Engine, error) {
	switch name {
	case "apple-container":
		return &applecontainer.Engine{Debug: os.Getenv("PADDOCK_DEBUG") == "1"}, nil
	}
	return nil, fmt.Errorf("unsupported engine %q", name)
}
