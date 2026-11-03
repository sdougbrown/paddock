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

	"github.com/sdougbrown/cli-contract"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/engine/applecontainer"
	"github.com/sdougbrown/paddock/internal/engine/podman"
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
	if code, handled := podman.Dispatch(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(clicontract.Run(app(), paddockAdapter{}, os.Args[1:], os.Stdout, os.Stderr))
}

// planFlagDecls adds the shared flags to a command's own list.
func planFlagDecls(cmd *clicontract.Command) {
	cmd.Flags = append(cmd.Flags, []clicontract.Flag{
		{Name: "profile", Kind: clicontract.String, Usage: "profile name (required)"},
		{Name: "spec", Kind: clicontract.String, Usage: "spec ref (default: from git origin, else profile default_spec)"},
		{Name: "workspace", Kind: clicontract.String, Usage: "workspace directory (default: current directory)"},
	}...)
}

// app assembles the declared CLI. Dispatch errors carry the app-name prefix:
// "paddock plan: unknown flag".
func app() *clicontract.App {
	return &clicontract.App{
		Name:        "paddock",
		ErrorPrefix: "paddock",
		BareHelp:    true,
		RootShort:   "Run one command in a sandbox with resolved mounts, limits, and network policy",
		RootLong: `paddock runs one command in a sandbox with resolved mounts, limits, and network policy.

Run and setup exit with the sandboxed command's own exit code.
Operational failures exit 1; usage errors exit 2.`,
		Commands: []*clicontract.Command{
			newPlanCmd(),
			newRunCmd(),
			newSetupCmd(),
			newVersionCmd(),
		},
	}
}

// paddockAdapter implements the result vocabulary: handlers print their own
// output; failures are silent on the streams (exit codes carry the
// outcome), usage errors exit 2, operational failures exit 1, and the
// sandboxed command's own exit code passes through.
type paddockAdapter struct{}

// ErrorResult returns nothing: dispatch failures write only their stderr
// diagnostic.
func (paddockAdapter) ErrorResult(command string, err error) any { return nil }

// Write writes plan JSON when the handler resolved a plan; other results
// print as their version line.
func (paddockAdapter) Write(w io.Writer, result any) error {
	switch r := result.(type) {
	case *plan.Plan:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	case versionResult:
		fmt.Fprintf(w, "paddock %s\n", r.version)
		return nil
	case nil:
		return nil
	default:
		return fmt.Errorf("paddock: unhandled result type %T", result)
	}
}

// ExitCode routes: sandboxed exit codes pass through verbatim, usage errors
// exit 2, operational failures exit 1, success exits 0.
func (paddockAdapter) ExitCode(inv clicontract.Invocation, result any, handlerErr error) int {
	var ec exitCodeError
	if errors.As(handlerErr, &ec) {
		return int(ec)
	}
	var ue *clicontract.UsageError
	if errors.As(handlerErr, &ue) {
		return 2
	}
	if handlerErr != nil {
		return 1
	}
	return 0
}

// Deadline passes the context through: the sandbox engines own their
// runtime.
func (paddockAdapter) Deadline(ctx context.Context, inv clicontract.Invocation) (context.Context, context.CancelFunc) {
	return ctx, func() {}
}

// Validate reports no pre-flight caller errors; guards run in the handlers.
func (paddockAdapter) Validate(inv clicontract.Invocation) error { return nil }

// BeforeHandler runs no pre-flight diagnostics.
func (paddockAdapter) BeforeHandler(inv clicontract.Invocation, stderr io.Writer) {}

// HandlerDiagnostic silences the diagnostic for sandboxed exit-code
// passthrough (the exit code carries the outcome) and prefixes the rest
// with the command path.
func (paddockAdapter) HandlerDiagnostic(inv clicontract.Invocation, err error) string {
	var ec exitCodeError
	if errors.As(err, &ec) {
		return ""
	}
	if inv.Command == "paddock" {
		return fmt.Sprintf("paddock: %v", err)
	}
	return fmt.Sprintf("paddock %s: %v", inv.Command, err)
}

// exitCodeError carries a sandboxed command's exit code out of a handler;
// the adapter returns it as the process exit code without printing anything.
type exitCodeError int

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// versionResult is written by the version command.
type versionResult struct{ version string }

// newPlanCmd resolves a plan and prints it as JSON.
func newPlanCmd() *clicontract.Command {
	cmd := &clicontract.Command{
		Name:  "plan",
		Short: "resolve the profile, spec, and workspace and print the Plan as JSON",
		Positionals: []clicontract.Positional{
			{Name: "command", Variadic: true},
		},
		NoInterspersed: true,
		Handle: func(ctx context.Context, inv clicontract.Invocation, stderr io.Writer) (any, error) {
			if inv.Flags["profile"] == "" {
				return nil, errors.New("--profile is required")
			}
			o := resolve.Options{
				Profile:   inv.Flags["profile"],
				Spec:      inv.Flags["spec"],
				Workspace: inv.Flags["workspace"],
				Phase:     inv.Flags["phase"],
				Argv:      inv.Args,
			}
			p, err := resolvePlan(o)
			if err != nil {
				return nil, err
			}
			return p, nil
		},
	}
	cmd.Flags = append(cmd.Flags, clicontract.Flag{Name: "phase", Kind: clicontract.String, Usage: "run or setup"})
	planFlagDecls(cmd)
	return cmd
}

// newRunCmd resolves a plan and runs it.
func newRunCmd() *clicontract.Command {
	cmd := &clicontract.Command{
		Name:  "run",
		Short: "run the resolved command in the sandbox, or the in-guest checks with --verify",
		Positionals: []clicontract.Positional{
			{Name: "command", Variadic: true},
		},
		NoInterspersed: true,
		Handle: func(ctx context.Context, inv clicontract.Invocation, stderr io.Writer) (any, error) {
			if inv.Flags["profile"] == "" {
				return nil, errors.New("--profile is required")
			}
			if len(inv.Args) == 0 && !inv.Bools["verify"] {
				return nil, errors.New("a command or --verify is required")
			}
			if len(inv.Args) > 0 && inv.Bools["verify"] {
				return nil, errors.New("--verify takes no command")
			}
			o := resolve.Options{
				Profile:   inv.Flags["profile"],
				Spec:      inv.Flags["spec"],
				Workspace: inv.Flags["workspace"],
				Phase:     "run",
				Argv:      inv.Args,
			}
			p, err := resolvePlan(o)
			if err != nil {
				return nil, err
			}
			eng, err := newEngine(p.Engine)
			if err != nil {
				return nil, err
			}
			code, err := eng.Run(ctx, p, engine.Options{Verify: inv.Bools["verify"]})
			if err != nil {
				return nil, err
			}
			if code != 0 {
				return nil, exitCodeError(code)
			}
			return nil, nil
		},
	}
	cmd.Flags = append(cmd.Flags, clicontract.Flag{Name: "verify", Kind: clicontract.Bool, Usage: "run the in-guest isolation checks instead of a command"})
	planFlagDecls(cmd)
	return cmd
}

// newSetupCmd runs the spec's setup command in a paddock with the dependency
// cache mounted.
func newSetupCmd() *clicontract.Command {
	cmd := &clicontract.Command{
		Name:  "setup",
		Short: "run the spec's setup command with the dependency cache mounted",
		Positionals: []clicontract.Positional{
			{Name: "command", Variadic: true},
		},
		Handle: func(ctx context.Context, inv clicontract.Invocation, stderr io.Writer) (any, error) {
			if inv.Flags["profile"] == "" {
				return nil, errors.New("--profile is required")
			}
			o := resolve.Options{
				Profile:   inv.Flags["profile"],
				Spec:      inv.Flags["spec"],
				Workspace: inv.Flags["workspace"],
				Phase:     plan.PhaseSetup,
				Argv:      inv.Args,
			}
			p, err := resolvePlan(o)
			if err != nil {
				return nil, err
			}
			eng, err := newEngine(p.Engine)
			if err != nil {
				return nil, err
			}
			code, err := runSetup(ctx, eng, p, stderr)
			if err != nil {
				return nil, err
			}
			if code != 0 {
				return nil, exitCodeError(code)
			}
			return nil, nil
		},
	}
	planFlagDecls(cmd)
	return cmd
}

// newVersionCmd prints the version and exits.
func newVersionCmd() *clicontract.Command {
	return &clicontract.Command{
		Name:  "version",
		Short: "print the version and exit",
		Handle: func(ctx context.Context, inv clicontract.Invocation, stderr io.Writer) (any, error) {
			return versionResult{version: version}, nil
		},
	}
}

// run executes the command line and returns the process exit code, the
// test-facing shape of Run.
func run(args []string, stdout, stderr io.Writer) int {
	return clicontract.Run(app(), paddockAdapter{}, args, stdout, stderr)
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
