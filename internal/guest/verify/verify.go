// Package verify runs the in-guest checks behind `paddock run --verify`.
// They run as the agent uid, after the network policy is committed and
// privileges are dropped, and probe the boundary from the agent's side.
//
// NetworkChecks returns the network half. Callers append their own Check
// values (for example read-only mount checks) and pass the whole list to
// Run, then print the results with Write.
package verify

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Check is one named verification. Run returns a short description of what
// it observed, and a non-nil error when the check fails.
type Check struct {
	Name string
	Run  func(ctx context.Context) (detail string, err error)
}

// Result is the outcome of one Check.
type Result struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// Run runs checks in order and returns one Result per check. A check that
// panics is a failure.
func Run(ctx context.Context, checks []Check) []Result {
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		out = append(out, runOne(ctx, c))
	}
	return out
}

func runOne(ctx context.Context, c Check) (r Result) {
	r.Name = c.Name
	defer func() {
		if p := recover(); p != nil {
			r.Pass = false
			r.Detail = fmt.Sprintf("check panicked: %v", p)
		}
	}()
	start := time.Now()
	detail, err := c.Run(ctx)
	elapsed := time.Since(start).Round(time.Millisecond)
	switch {
	case err != nil && detail != "":
		r.Detail = fmt.Sprintf("%s: %v (%v)", detail, err, elapsed)
	case err != nil:
		r.Detail = fmt.Sprintf("%v (%v)", err, elapsed)
	default:
		r.Detail = fmt.Sprintf("%s (%v)", detail, elapsed)
	}
	r.Pass = err == nil
	return r
}

// Write prints one line per result and reports whether all passed.
func Write(w io.Writer, results []Result) bool {
	all := true
	for _, r := range results {
		status := "PASS"
		if !r.Pass {
			status = "FAIL"
			all = false
		}
		fmt.Fprintf(w, "%s %s: %s\n", status, r.Name, r.Detail)
	}
	return all
}
