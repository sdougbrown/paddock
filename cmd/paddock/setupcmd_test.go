package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/plan"
)

func TestSummaryLine(t *testing.T) {
	got := summaryLine(&plan.SetupPlan{CacheHost: "/c/x"}, true, 1500*time.Millisecond, 3)
	for _, want := range []string{"cache hit", "/c/x", "1.5s", "exit 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
}
