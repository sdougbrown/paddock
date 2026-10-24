package netpolicy

import (
	"context"
	"testing"
)

func TestApplyRejectsEmptyRuleset(t *testing.T) {
	if err := Apply(context.Background(), ""); err == nil {
		t.Fatal("Apply with an empty ruleset returned nil")
	}
}
