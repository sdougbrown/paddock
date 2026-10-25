package images

import "testing"

func TestClaudeCodeVersionComesFromDockerfile(t *testing.T) {
	for _, tc := range []struct {
		dockerfile, want string
		ok               bool
	}{
		{"RUN npm install -g @anthropic-ai/claude-code@2.1.303 \\\n    && npm cache clean --force\n", "2.1.303", true},
		{"RUN npm install -g @anthropic-ai/claude-code@2.2.0\n", "2.2.0", true},
		{"RUN npm install -g @anthropic-ai/claude-code\n", "", false},
		{"RUN npm i -g @anthropic-ai/claude-code@1.0.0 @anthropic-ai/claude-code@2.0.0\n", "", false},
	} {
		got, err := claudeCodeVersion([]byte(tc.dockerfile))
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("claudeCodeVersion(%q) = %q, %v; want %q", tc.dockerfile, got, err, tc.want)
		}
	}
	if want, _ := claudeCodeVersion(BaseDockerfile); ClaudeCodeVersion != want || want == "" {
		t.Errorf("ClaudeCodeVersion = %q, base Dockerfile installs %q", ClaudeCodeVersion, want)
	}
}
