package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

type recorder struct {
	steps   []string
	failOn  string       // step name whose every call fails
	failAt  map[int]bool // apply calls that fail, counted from 1
	applied []string
	ctxErrs []error  // ctx.Err() seen by each apply call
	started []Policy // the policy each start step got
	runAs   *[4]int  // the proxy's uids; nil means the policy's ProxyUID
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

// proxyPID is the process id the recorded start step reports.
const proxyPID = 4242

func (r *recorder) startProxy(_ context.Context, p Policy) (int, error) {
	r.steps = append(r.steps, "start")
	r.started = append(r.started, p)
	if r.failOn == "start" {
		return 0, errors.New("start failed")
	}
	return proxyPID, nil
}

func (r *recorder) uids(pid int) ([4]int, error) {
	if pid != proxyPID {
		return [4]int{}, fmt.Errorf("uids of pid %d; the proxy is %d", pid, proxyPID)
	}
	if r.runAs != nil {
		return *r.runAs, nil
	}
	u := r.started[len(r.started)-1].ProxyUID
	return [4]int{u, u, u, u}, nil
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
				StartProxy: r.startProxy, ProcessUIDs: r.uids,
				Lifeline: r.step("lifeline"),
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
				StartProxy: r.startProxy, ProcessUIDs: r.uids,
				Lifeline: r.step("lifeline"),
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
		StartProxy: r.startProxy, ProcessUIDs: r.uids,
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
	err := Commit(context.Background(), p, CommitOptions{Apply: r.apply, StartProxy: r.startProxy, ProcessUIDs: r.uids, Lifeline: r.step("lifeline")})
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

// The deny-all loads that start a commit get a live context, as the
// rollback does: a cancelled caller must not keep a previous, more open
// policy in force.
func TestCommitDenyAllOutlivesCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, mode := range []Mode{ModeNone, ModeAllowlist} {
		t.Run(string(mode), func(t *testing.T) {
			r := &recorder{}
			p := basePolicy()
			p.Mode = mode
			_ = Commit(ctx, p, CommitOptions{Apply: r.apply, StartProxy: r.startProxy, ProcessUIDs: r.uids, Lifeline: r.step("lifeline")})
			if len(r.ctxErrs) == 0 || r.ctxErrs[0] != nil {
				t.Fatalf("first deny-all load got context error %v; want a live context", r.ctxErrs)
			}
		})
	}
}

// Mode none is only deny-all, so a failed load must reach the caller.
func TestCommitNoneReportsFailedDenyAll(t *testing.T) {
	r := &recorder{failOn: "apply deny-all"}
	p := basePolicy()
	p.Mode = ModeNone
	err := Commit(context.Background(), p, CommitOptions{Apply: r.apply})
	if err == nil || !strings.Contains(err.Error(), "apply deny-all: nft failed") {
		t.Fatalf("err = %v; want the failed deny-all load", err)
	}
}

// An unknown mode has no ruleset, so Commit must load deny-all and fail.
func TestCommitUnknownModeDeniesAll(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		r := &recorder{}
		p := basePolicy()
		p.Mode = "open"
		err := Commit(context.Background(), p, CommitOptions{Apply: r.apply})
		if err == nil {
			t.Fatal("Commit with mode open returned nil")
		}
		if got := strings.Join(r.steps, ","); got != "apply deny-all" {
			t.Errorf("steps = %s; want only deny-all", got)
		}
		if r.last() != "deny-all" {
			t.Errorf("last applied = %q; want deny-all", r.last())
		}
	})
}

// Commit hands StartProxy the policy whose ruleset it loads, so the proxy
// binds the address and runs as the uid that ruleset exempts.
func TestCommitPassesPolicyToStartProxy(t *testing.T) {
	r := &recorder{}
	if err := Commit(context.Background(), basePolicy(), CommitOptions{Apply: r.apply, StartProxy: r.startProxy, ProcessUIDs: r.uids, Lifeline: r.step("lifeline")}); err != nil {
		t.Fatal(err)
	}
	if len(r.started) != 1 || !reflect.DeepEqual(r.started[0], basePolicy()) {
		t.Fatalf("StartProxy got %+v; want %+v", r.started, basePolicy())
	}
}

// The ruleset exempts only ProxyUID from the tcp/443 redirect. A proxy
// running as any other uid would leave ProxyUID's egress open, so Commit
// must fall back to deny-all before loading the allowlist.
func TestCommitProxyUIDMismatchFailsClosed(t *testing.T) {
	for name, uids := range map[string][4]int{
		"all":       {1000, 1000, 1000, 1000},
		"real":      {1000, 970, 970, 970},
		"effective": {970, 1000, 970, 970},
		"saved":     {970, 970, 1000, 970},
		"fs":        {970, 970, 970, 1000},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{runAs: &uids}
			err := Commit(context.Background(), basePolicy(), CommitOptions{Apply: r.apply, StartProxy: r.startProxy, ProcessUIDs: r.uids, Lifeline: r.step("lifeline")})
			if err == nil || !strings.Contains(err.Error(), "uid 970") {
				t.Fatalf("err = %v; want a mismatch naming uid 970", err)
			}
			if got := strings.Join(r.steps, ","); got != "apply deny-all,start,apply deny-all" {
				t.Errorf("steps = %s; want deny-all, start, deny-all", got)
			}
			if r.last() != "deny-all" {
				t.Errorf("last applied = %q; want deny-all", r.last())
			}
		})
	}
}

func TestProcStatusUIDs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	got, err := procStatusUIDs(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != os.Getuid() || got[1] != os.Geteuid() {
		t.Fatalf("uids = %v; want real %d and effective %d", got, os.Getuid(), os.Geteuid())
	}
	if _, err := procStatusUIDs(-1); err == nil {
		t.Fatal("procStatusUIDs(-1) succeeded")
	}
	if os.Geteuid() != 0 {
		return
	}
	// As root, start a process as another uid and read its uids, not ours.
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 4242, Gid: 4242}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := checkProxyUIDs(procStatusUIDs, cmd.Process.Pid, 4242); err != nil {
		t.Errorf("uid 4242 process: %v", err)
	}
	if err := checkProxyUIDs(procStatusUIDs, cmd.Process.Pid, 970); err == nil {
		t.Error("uid 4242 process passed as uid 970")
	}
}
