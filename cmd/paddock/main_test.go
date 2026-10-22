package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/plan"
)

func TestPlanCommand(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "internal", "resolve", "testdata", "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADDOCK_CONFIG_DIR", root)
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"plan", "--profile", "fixer", "--spec", "afresh", "--workspace", ws, "--", "claude"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var p plan.Plan
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		t.Fatalf("stdout is not a plan: %v\n%s", err, stdout.String())
	}
	if strings.Join(p.Argv, " ") != "claude" || p.Phase != "run" {
		t.Errorf("argv %q phase %q", p.Argv, p.Phase)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"plan", "--", "claude"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "--profile is required") {
		t.Errorf("missing profile: exit %d, stderr %q", code, stderr.String())
	}
}
