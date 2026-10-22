package images

import (
	"strings"
	"testing"
)

func TestDockerfilePinsClaudeCodeVersion(t *testing.T) {
	want := "@anthropic-ai/claude-code@" + ClaudeCodeVersion
	if !strings.Contains(string(BaseDockerfile), want) {
		t.Fatalf("base Dockerfile does not install %s", want)
	}
}
