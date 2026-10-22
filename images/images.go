// Package images embeds the Dockerfile of the base image every paddock layer chain starts from.
package images

import _ "embed"

// ClaudeCodeVersion is the Claude Code release installed in the base image.
const ClaudeCodeVersion = "2.1.303"

// BaseDockerfile is the base image's Dockerfile.
//
//go:embed paddock-base/Dockerfile
var BaseDockerfile []byte
