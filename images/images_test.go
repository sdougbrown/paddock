package images

import (
	"regexp"
	"testing"
)

func TestDockerfilePinsClaudeCodeVersion(t *testing.T) {
	m := regexp.MustCompile(`@anthropic-ai/claude-code@(\S+)`).FindAllSubmatch(BaseDockerfile, -1)
	if len(m) != 1 || string(m[0][1]) != ClaudeCodeVersion {
		t.Fatalf("base Dockerfile installs claude-code %q, want exactly one install of %s", m, ClaudeCodeVersion)
	}
}
