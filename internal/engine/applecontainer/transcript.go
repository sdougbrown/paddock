package applecontainer

import (
	"path/filepath"
	"strconv"
	"unicode/utf16"
)

// maxProjectDirName is the length, in UTF-16 code units, past which Claude
// Code truncates a project directory name and appends a hash.
const maxProjectDirName = 200

// transcriptDir returns the directory Claude Code keeps session transcripts
// for workdir in: <home>/.claude/projects/<projectDirName(workdir)>.
func transcriptDir(home, workdir string) string {
	return filepath.Join(home, ".claude", "projects", projectDirName(workdir))
}

// projectDirName ports the project directory naming of the Claude Code
// release in the base image (images.ClaudeCodeVersion). JavaScript strings are
// UTF-16, so the name is built per UTF-16 code unit: every unit outside
// [A-Za-z0-9] becomes "-", and a character outside the BMP becomes two.
// A name longer than maxProjectDirName units keeps its first
// maxProjectDirName units and gains "-<base36 of |hash|>", where hash is the
// 32-bit h = h*31 + unit over the workdir's own code units.
func projectDirName(workdir string) string {
	units := utf16.Encode([]rune(workdir))
	name := make([]byte, len(units))
	var hash int32
	for i, u := range units {
		if u < 0x80 && isAlnum(byte(u)) {
			name[i] = byte(u)
		} else {
			name[i] = '-'
		}
		hash = hash<<5 - hash + int32(u)
	}
	if len(name) <= maxProjectDirName {
		return string(name)
	}
	abs := int64(hash)
	if abs < 0 {
		abs = -abs
	}
	return string(name[:maxProjectDirName]) + "-" + strconv.FormatInt(abs, 36)
}

func isAlnum(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}
