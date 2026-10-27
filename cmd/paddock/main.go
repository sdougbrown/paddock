// Command paddock runs one command in a sandbox with explicit mounts, limits,
// and network policy. Invoked as paddock-init it is the guest entrypoint.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine/applecontainer"
	"github.com/sdougbrown/paddock/internal/guest/pinit"
	"github.com/sdougbrown/paddock/internal/plan"
	"github.com/sdougbrown/paddock/internal/resolve"
)

// version is set at release time with -ldflags "-X main.version=<v>".
var version = "dev"

func main() {
	if filepath.Base(os.Args[0]) == "paddock-init" {
		os.Exit(pinit.Main(os.Args[1:]))
	}
	if code, handled := applecontainer.Dispatch(os.Args[1:]); handled {
		os.Exit(code)
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
		code, err := planCmd(args[1:], stdout, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "paddock plan:", err)
		}
		return code
	case "run":
		code, err := runCmd(args[1:], stderr)
		if err != nil {
			fmt.Fprintln(stderr, "paddock run:", err)
		}
		return code
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

// planCmd resolves a plan and prints it as JSON, returning the exit code.
func planCmd(args []string, stdout, stderr io.Writer) (int, error) {
	var o resolve.Options
	fs := planFlagSet("plan", stderr, &o)
	fs.StringVar(&o.Phase, "phase", "run", "run or setup")
	if err := fs.Parse(args); err != nil {
		return parseExitCode(err), nil
	}
	if o.Profile == "" {
		return 1, fmt.Errorf("--profile is required")
	}
	o.Argv = fs.Args()
	p, err := resolvePlan(o)
	if err != nil {
		return 1, err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return 1, err
	}
	return 0, nil
}

// planFlagSet returns a flag set for the named command with the flags plan,
// run, and setup share: --profile, --spec, and --workspace, stored in o. A
// parse error or -h is reported by the flag set on output; the commands then
// exit with parseExitCode without printing it again.
func planFlagSet(name string, output io.Writer, o *resolve.Options) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&o.Profile, "profile", "", "profile name (required)")
	fs.StringVar(&o.Spec, "spec", "", "spec ref (default: from git origin, else profile default_spec)")
	fs.StringVar(&o.Workspace, "workspace", "", "workspace directory (default: current directory)")
	return fs
}

// parseExitCode is a command's exit code after its flag set failed to
// parse: 0 for -h or -help, as flag.ExitOnError does, and 2 otherwise.
func parseExitCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// resolvePlan resolves o against the user's config root.
func resolvePlan(o resolve.Options) (*plan.Plan, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home directory: %w", err)
	}
	root, err := config.ResolveRoot(os.Getenv, home)
	if err != nil {
		return nil, err
	}
	return resolve.New(root, home).Resolve(o)
}
