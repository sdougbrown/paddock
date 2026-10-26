// Package images embeds the Dockerfile of the base image every paddock layer chain starts from.
package images

import (
	_ "embed"
	"fmt"
	"regexp"
)

// BaseDockerfile is the base image's Dockerfile.
//
//go:embed paddock-base/Dockerfile
var BaseDockerfile []byte

// ClaudeCodeVersion is the Claude Code release installed in the base image,
// read from the one `@anthropic-ai/claude-code@<version>` install in
// BaseDockerfile.
var ClaudeCodeVersion = mustClaudeCodeVersion(BaseDockerfile)

var claudeCodeInstall = regexp.MustCompile(`@anthropic-ai/claude-code@(\S+)`)

func claudeCodeVersion(dockerfile []byte) (string, error) {
	m := claudeCodeInstall.FindAllSubmatch(dockerfile, -1)
	if len(m) != 1 {
		return "", fmt.Errorf("want one pinned @anthropic-ai/claude-code install, found %d", len(m))
	}
	return string(m[0][1]), nil
}

func mustClaudeCodeVersion(dockerfile []byte) string {
	v, err := claudeCodeVersion(dockerfile)
	if err != nil {
		panic("images: base Dockerfile: " + err.Error())
	}
	return v
}
