package main

import (
	"testing"
	"time"

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
