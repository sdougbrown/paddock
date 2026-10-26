package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/plan"
)

func TestSummaryLine(t *testing.T) {
	tests := []struct {
		name  string
		setup *plan.SetupPlan
		hit   bool
		want  string
	}{
		{"hit", &plan.SetupPlan{CacheHost: "/c/x"}, true, "paddock setup: cache hit, /c/x, 1.5s, exit 3"},
		{"miss", &plan.SetupPlan{CacheHost: "/c/x"}, false, "paddock setup: cache miss, /c/x, 1.5s, exit 3"},
		{"no cache", &plan.SetupPlan{}, false, "paddock setup: no cache, 1.5s, exit 3"},
		{"no cache ignores hit", &plan.SetupPlan{}, true, "paddock setup: no cache, 1.5s, exit 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summaryLine(tt.setup, tt.hit, 1500*time.Millisecond, 3); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// fakeEngine returns code and err from Run and records whether the setup
// cache lock was held while it ran.
type fakeEngine struct {
	code        int
	err         error
	lockedInRun bool
}

func (f *fakeEngine) Run(_ context.Context, p *plan.Plan, _ engine.Options) (int, error) {
	lf, err := os.Open(p.Setup.CacheHost + ".lock")
	if err == nil {
		f.lockedInRun = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == syscall.EWOULDBLOCK
		lf.Close()
	}
	return f.code, f.err
}

// TestRunSetupCompletesCacheOnlyOnSuccess runs setup five times against one
// cache. Only a run that exits 0 makes the next run a cache hit, and every
// run holds the cache lock while the engine runs and releases it after.
func TestRunSetupCompletesCacheOnlyOnSuccess(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "spec", "0123456789abcdef")
	p := &plan.Plan{Setup: &plan.SetupPlan{CacheHost: cache}}
	runErr := errors.New("engine failed")
	steps := []struct {
		eng      *fakeEngine
		wantCode int
		wantErr  error
		wantHit  bool
	}{
		{&fakeEngine{code: 3}, 3, nil, false},
		{&fakeEngine{code: 0}, 0, nil, false},
		{&fakeEngine{err: runErr}, 0, runErr, true},
		{&fakeEngine{code: 0}, 0, nil, false},
		{&fakeEngine{code: 0}, 0, nil, true},
	}
	for i, s := range steps {
		var stderr bytes.Buffer
		code, err := runSetup(t.Context(), s.eng, p, &stderr)
		if code != s.wantCode || !errors.Is(err, s.wantErr) {
			t.Fatalf("step %d: got %d, %v; want %d, %v", i, code, err, s.wantCode, s.wantErr)
		}
		state := "miss"
		if s.wantHit {
			state = "hit"
		}
		if want := "paddock setup: cache " + state + ", " + cache + ","; !strings.HasPrefix(stderr.String(), want) {
			t.Errorf("step %d: summary %q, want prefix %q", i, stderr.String(), want)
		}
		if !s.eng.lockedInRun {
			t.Errorf("step %d: cache lock not held during Run", i)
		}
	}
}
