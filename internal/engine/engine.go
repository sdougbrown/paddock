// Package engine defines the interface between a resolved plan and the runtime that executes it.
package engine

import (
	"context"

	"github.com/sdougbrown/paddock/internal/plan"
)

// Options are per-invocation settings that are not part of the plan.
type Options struct {
	// Verify runs the in-guest checks instead of the plan's argv.
	Verify bool
}

// Engine runs one plan to completion.
type Engine interface {
	// Run executes the plan and returns the command's exit code
	// (128+signal when it was killed by a signal). An error means the run
	// could not be started or supervised, not that the command failed.
	Run(ctx context.Context, p *plan.Plan, o Options) (exitCode int, err error)
}
