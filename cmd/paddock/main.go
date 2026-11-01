// Command paddock runs one command in a sandbox with explicit mounts, limits,
// and network policy. Invoked as paddock-init it is the guest entrypoint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine"
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

// usageError marks a flag-parse or command-dispatch failure, which exits 2.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }

// exitCodeError carries a sandboxed command's exit code out of a RunE; run
// returns it as the process exit code without printing anything.
type exitCodeError int

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// run executes the command line and returns the process exit code: 0 on
// success, 2 for a usage error, 1 for an operational failure, and the
// sandboxed command's own exit code for run and setup.
func run(args []string, stdout, stderr io.Writer) int {
	root := newRootCmd(stdout, stderr)
	root.SetArgs(args)
	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	name := "paddock"
	if cmd != nil && cmd != root {
		name = "paddock " + cmd.Name()
	}
	var ue usageError
	var ec exitCodeError
	switch {
	case errors.As(err, &ec):
		return int(ec)
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "%s: %v\n", name, ue.err)
		return 2
	case cmd == nil || cmd == root:
		// A dispatch failure at the root, such as an unknown command.
		fmt.Fprintf(stderr, "paddock: %v\n", err)
		return 2
	default:
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
}

// newRootCmd builds the command tree. Help output goes to stdout and errors
// to stderr; SilenceErrors and SilenceUsage keep printing in run's hands so
// the exit-code contract holds.
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "paddock",
		Short: "Run one command in a sandbox with resolved mounts, limits, and network policy",
		Long: `paddock runs one command in a sandbox with resolved mounts, limits, and network policy.

Run and setup exit with the sandboxed command's own exit code.
Operational failures exit 1; usage errors exit 2.`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return usageError{err}
	})
	root.AddCommand(newPlanCmd(stdout), newRunCmd(), newSetupCmd(), newVersionCmd(stdout))
	return root
}

// newPlanCmd resolves a plan and prints it as JSON.
func newPlanCmd(stdout io.Writer) *cobra.Command {
	var o resolve.Options
	cmd := &cobra.Command{
		Use:   "plan [--] command [args...]",
		Short: "resolve the profile, spec, and workspace and print the Plan as JSON",
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.Profile == "" {
				return errors.New("--profile is required")
			}
			o.Argv = args
			p, err := resolvePlan(o)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(p)
		},
	}
	cmd.Flags().StringVar(&o.Phase, "phase", "run", "run or setup")
	addPlanFlags(cmd, &o)
	return cmd
}

// newRunCmd resolves a plan and runs it.
func newRunCmd() *cobra.Command {
	var o resolve.Options
	var verify bool
	cmd := &cobra.Command{
		Use:   "run [--flags] [--] command [args...]",
		Short: "run the resolved command in the sandbox, or the in-guest checks with --verify",
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.Profile == "" {
				return errors.New("--profile is required")
			}
			if len(args) == 0 && !verify {
				return errors.New("a command or --verify is required")
			}
			if len(args) > 0 && verify {
				return errors.New("--verify takes no command")
			}
			o.Argv = args
			p, err := resolvePlan(o)
			if err != nil {
				return err
			}
			eng, err := newEngine(p.Engine)
			if err != nil {
				return err
			}
			code, err := eng.Run(context.Background(), p, engine.Options{Verify: verify})
			if err != nil {
				return err
			}
			if code != 0 {
				return exitCodeError(code)
			}
			return nil
		},
	}
	addPlanFlags(cmd, &o)
	cmd.Flags().BoolVar(&verify, "verify", false, "run the in-guest isolation checks instead of a command")
	return cmd
}

// newSetupCmd runs the spec's setup command in a paddock with the dependency
// cache mounted.
func newSetupCmd() *cobra.Command {
	o := resolve.Options{Phase: plan.PhaseSetup}
	cmd := &cobra.Command{
		Use:   "setup [--flags]",
		Short: "run the spec's setup command with the dependency cache mounted",
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.Profile == "" {
				return errors.New("--profile is required")
			}
			// The resolver rejects a command in the setup phase.
			o.Argv = args
			p, err := resolvePlan(o)
			if err != nil {
				return err
			}
			eng, err := newEngine(p.Engine)
			if err != nil {
				return err
			}
			code, err := runSetup(context.Background(), eng, p, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if code != 0 {
				return exitCodeError(code)
			}
			return nil
		},
	}
	addPlanFlags(cmd, &o)
	return cmd
}

func newVersionCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print the version and exit",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "paddock", version)
			return nil
		},
	}
}

// addPlanFlags adds the flags plan, run, and setup share: --profile,
// --spec, and --workspace, stored in o.
func addPlanFlags(cmd *cobra.Command, o *resolve.Options) {
	cmd.Flags().StringVar(&o.Profile, "profile", "", "profile name (required)")
	cmd.Flags().StringVar(&o.Spec, "spec", "", "spec ref (default: from git origin, else profile default_spec)")
	cmd.Flags().StringVar(&o.Workspace, "workspace", "", "workspace directory (default: current directory)")
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
