package applecontainer

import (
	"path/filepath"
	"regexp"
)

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9-]`)

// transcriptDir returns the directory Claude Code keeps session transcripts
// for workdir in: <home>/.claude/projects/<encoded workdir>. The encoding
// replaces every character outside [A-Za-z0-9-] with "-".
func transcriptDir(home, workdir string) string {
	return filepath.Join(home, ".claude", "projects", nonAlnum.ReplaceAllString(workdir, "-"))
}
