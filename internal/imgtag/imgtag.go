// Package imgtag derives content-addressed image tags for the spec chain.
package imgtag

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Hash is a hex digest over the Dockerfile bytes, the context tree
// (relative paths, modes, and contents or link targets, sorted), and the
// parent tag. Symlinks inside the context are hashed by their link target, not
// the content they point at: a Dockerfile COPY of a directory copies links as
// links, so retargeting a link changes the image and editing its target's
// content does not. A symlinked contextDir is resolved first. An empty contextDir
// contributes an empty tree.
func Hash(dockerfile []byte, contextDir, parentTag string) (string, error) {
	h := sha256.New()
	writeField(h, "dockerfile", dockerfile)
	if contextDir != "" {
		resolved, err := filepath.EvalSymlinks(contextDir)
		if err != nil {
			return "", fmt.Errorf("hash context %s: %w", contextDir, err)
		}
		contextDir = resolved
		err = filepath.WalkDir(contextDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(contextDir, p)
			if err != nil || rel == "." {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			var body []byte
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(p)
				if err != nil {
					return err
				}
				body = []byte(target)
			case info.Mode().IsRegular():
				if body, err = os.ReadFile(p); err != nil {
					return err
				}
			}
			writeField(h, "path", []byte(filepath.ToSlash(rel)+"\x00"+info.Mode().String()))
			writeField(h, "body", body)
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("hash context %s: %w", contextDir, err)
		}
	}
	writeField(h, "parent", []byte(parentTag))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeField hashes a length-prefixed field so adjacent fields cannot blur.
func writeField(h io.Writer, label string, b []byte) {
	fmt.Fprintf(h, "%s:%d:", label, len(b))
	h.Write(b)
}

// LayerTag is the image tag for a spec layer:
// paddock/<lowercased spec with "/" as "-">-<ref8>:<hash12>, where ref8 is the
// first 8 hex digits of the sha256 of the spec ref as given. The digest keeps
// specs whose readable names coincide (x/y and x-y, case variants, or a spec
// named base) in separate repositories, so pruning one never touches another.
func LayerTag(spec, hash string) string {
	ref := sha256.Sum256([]byte(spec))
	return "paddock/" + strings.ReplaceAll(strings.ToLower(spec), "/", "-") + "-" + hex.EncodeToString(ref[:4]) + ":" + hash[:12]
}
