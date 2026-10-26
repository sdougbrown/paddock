package main

import (
	"context"
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
	var o resolve.Options
	var verify bool
	fs := planFlagSet("run", stderr, &o)
	fs.BoolVar(&verify, "verify", false, "run the in-guest isolation checks instead of a command")
	if err := fs.Parse(args); err != nil {
		return parseExitCode(err), nil
	}
	if o.Profile == "" {
		return 1, fmt.Errorf("--profile is required")
	}
	o.Argv = fs.Args()
	if len(o.Argv) == 0 && !verify {
		return 1, fmt.Errorf("a command or --verify is required")
	}
	if len(o.Argv) > 0 && verify {
		return 1, fmt.Errorf("--verify takes no command")
	}
	p, err := resolvePlan(o)
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
	case config.EngineAppleContainer:
		return &applecontainer.Engine{Debug: os.Getenv("PADDOCK_DEBUG") == "1"}, nil
	}
	return nil, fmt.Errorf("unsupported engine %q", name)
}
