// Command paddock runs one command in a sandbox with explicit mounts, limits,
// and network policy. Invoked as paddock-init it is the guest entrypoint.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine/applecontainer"
	"github.com/sdougbrown/paddock/internal/resolve"
)

const version = "0.1.0-dev"

func main() {
	if filepath.Base(os.Args[0]) == "paddock-init" {
		fmt.Fprintln(os.Stderr, "paddock-init: not implemented")
		os.Exit(2)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: paddock <plan|run|setup|version>")
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "paddock", version)
		return 0
	case "plan":
		if err := planCmd(args[1:], stdout); err != nil {
			fmt.Fprintln(stderr, "paddock plan:", err)
			return 1
		}
		return 0
	case "run":
		code, err := runCmd(args[1:], stderr)
		if err != nil {
			fmt.Fprintln(stderr, "paddock run:", err)
		}
		return code
	case applecontainer.WatchdogCommand:
		return applecontainer.RunWatchdog(args[1:])
	case "setup":
		code, err := setupCmd(args[1:], stderr)
		if err != nil {
			fmt.Fprintln(stderr, "paddock setup:", err)
		}
		return code
	}
	fmt.Fprintf(stderr, "paddock: unknown command %q\n", args[0])
	return 2
}

func planCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	var o resolve.Options
	fs.StringVar(&o.Profile, "profile", "", "profile name (required)")
	fs.StringVar(&o.Spec, "spec", "", "spec ref (default: from git origin, else profile default_spec)")
	fs.StringVar(&o.Workspace, "workspace", "", "workspace directory (default: current directory)")
	fs.StringVar(&o.Phase, "phase", "run", "run or setup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.Profile == "" {
		return fmt.Errorf("--profile is required")
	}
	o.Argv = fs.Args()
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home directory: %w", err)
	}
	root, err := config.ResolveRoot(os.Getenv, home)
	if err != nil {
		return err
	}
	p, err := resolve.New(root, home).Resolve(o)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}
