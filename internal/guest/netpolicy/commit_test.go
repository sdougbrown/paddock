package netpolicy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recorder struct {
	steps   []string
	failOn  string       // step name whose every call fails
	failAt  map[int]bool // apply calls that fail, counted from 1
	applied []string
	ctxErrs []error // ctx.Err() seen by each apply call
}

func (r *recorder) apply(ctx context.Context, rules string) error {
	name := "allowlist"
	if rules == DenyAll() {
		name = "deny-all"
	}
	r.steps = append(r.steps, "apply "+name)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	if r.failOn == "apply "+name || r.failAt[len(r.ctxErrs)] {
		return errors.New("nft failed")
	}
	r.applied = append(r.applied, name)
	return nil
}

func (r *recorder) step(name string) func(context.Context) error {
	return func(context.Context) error {
		r.steps = append(r.steps, name)
		if r.failOn == name {
			return errors.New(name + " failed")
		}
		return nil
	}
}

func (r *recorder) last() string {
	if len(r.applied) == 0 {
		return ""
	}
	return r.applied[len(r.applied)-1]
}

func TestCommitAllowlist(t *testing.T) {
	tests := []struct {
		failOn    string
		wantSteps string
		wantLast  string
		wantErr   string
	}{
		{"", "apply deny-all,start,apply allowlist,lifeline", "allowlist", ""},
		{"start", "apply deny-all,start,apply deny-all", "deny-all", "start proxy"},
		{"apply allowlist", "apply deny-all,start,apply allowlist,apply deny-all", "deny-all", "apply allowlist"},
		{"lifeline", "apply deny-all,start,apply allowlist,lifeline,apply deny-all", "deny-all", "lifeline"},
		{"apply deny-all", "apply deny-all", "", "apply deny-all"},
	}
	for _, tt := range tests {
		name := tt.failOn
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			r := &recorder{failOn: tt.failOn}
			err := Commit(context.Background(), basePolicy(), CommitOptions{
				Apply:      r.apply,
				StartProxy: r.step("start"),
				Lifeline:   r.step("lifeline"),
			})
			if got := strings.Join(r.steps, ","); got != tt.wantSteps {
				t.Errorf("steps = %s; want %s", got, tt.wantSteps)
			}
			if r.last() != tt.wantLast {
				t.Errorf("last applied = %q; want %q", r.last(), tt.wantLast)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v; want containing %q", err, tt.wantErr)
			}
		})
	}
}

// When the fail-closed deny-all itself fails, the allowlist may still be in
// force: Commit must report that along with the cause.
func TestCommitReportsFailedRollback(t *testing.T) {
	tests := []struct {
		name      string
		failOn    string
		failAt    map[int]bool
		wantSteps string
		wantLast  string
		wantCause string
	}{
		{"start", "start", map[int]bool{2: true}, "apply deny-all,start,apply deny-all", "deny-all", "start proxy"},
		{"apply allowlist", "", map[int]bool{2: true, 3: true}, "apply deny-all,start,apply allowlist,apply deny-all", "deny-all", "apply allowlist"},
		{"lifeline", "lifeline", map[int]bool{3: true}, "apply deny-all,start,apply allowlist,lifeline,apply deny-all", "allowlist", "lifeline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recorder{failOn: tt.failOn, failAt: tt.failAt}
			err := Commit(context.Background(), basePolicy(), CommitOptions{
				Apply:      r.apply,
				StartProxy: r.step("start"),
				Lifeline:   r.step("lifeline"),
			})
			if got := strings.Join(r.steps, ","); got != tt.wantSteps {
				t.Errorf("steps = %s; want %s", got, tt.wantSteps)
			}
			if r.last() != tt.wantLast {
				t.Errorf("last applied = %q; want %q", r.last(), tt.wantLast)
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantCause) || !strings.Contains(err.Error(), "fail-closed deny-all also failed") {
				t.Fatalf("err = %v; want the cause %q and the failed rollback", err, tt.wantCause)
			}
		})
	}
}

// A context cancelled during the lifeline must not stop the rollback: the
// deny-all load gets a context that is still live.
func TestCommitRollbackOutlivesCancel(t *testing.T) {
	r := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Commit(ctx, basePolicy(), CommitOptions{
		Apply:      r.apply,
		StartProxy: r.step("start"),
		Lifeline: func(ctx context.Context) error {
			cancel()
			return ctx.Err()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want the cancellation", err)
	}
	if got := strings.Join(r.steps, ","); got != "apply deny-all,start,apply allowlist,apply deny-all" {
		t.Errorf("steps = %s", got)
	}
	if r.last() != "deny-all" {
		t.Errorf("last applied = %q; want deny-all", r.last())
	}
	if last := r.ctxErrs[len(r.ctxErrs)-1]; last != nil {
		t.Errorf("rollback apply got a dead context: %v", last)
	}
}

func TestCommitInvalidPolicyFailsClosed(t *testing.T) {
	r := &recorder{}
	p := basePolicy()
	p.Resolvers = nil
	err := Commit(context.Background(), p, CommitOptions{Apply: r.apply, StartProxy: r.step("start"), Lifeline: r.step("lifeline")})
	if err == nil {
		t.Fatal("want error")
	}
	if got := strings.Join(r.steps, ","); got != "apply deny-all" {
		t.Errorf("steps = %s; want only deny-all", got)
	}
}

func TestCommitAllowlistNeedsHooks(t *testing.T) {
	r := &recorder{}
	err := Commit(context.Background(), basePolicy(), CommitOptions{Apply: r.apply})
	if err == nil || r.last() != "deny-all" {
		t.Fatalf("err = %v, last = %q; want error and deny-all", err, r.last())
	}
}

func TestCommitNoneAndHost(t *testing.T) {
	r := &recorder{}
	p := basePolicy()
	p.Mode = ModeNone
	if err := Commit(context.Background(), p, CommitOptions{Apply: r.apply}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.steps, ",") != "apply deny-all" {
		t.Errorf("none steps = %v", r.steps)
	}
	r = &recorder{}
	p.Mode = ModeHost
	if err := Commit(context.Background(), p, CommitOptions{Apply: r.apply}); err != nil {
		t.Fatal(err)
	}
	if len(r.steps) != 0 {
		t.Errorf("host steps = %v; want none", r.steps)
	}
}
