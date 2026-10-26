package applecontainer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sdougbrown/paddock/images"
	"github.com/sdougbrown/paddock/internal/plan"
)

// imageHash is a hex digest over the Dockerfile bytes, the context tree
// (relative paths, modes, and contents or link targets, sorted), and the
// parent tag. Symlinks inside the context are hashed by their link target, not
// the content they point at: a Dockerfile COPY of a directory copies links as
// links, so retargeting a link changes the image and editing its target's
// content does not. A symlinked contextDir is resolved first. An empty contextDir
// contributes an empty tree.
func imageHash(dockerfile []byte, contextDir, parentTag string) (string, error) {
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

// layerTag is the image tag for a spec layer:
// paddock/<lowercased spec with "/" as "-">-<ref8>:<hash12>, where ref8 is the
// first 8 hex digits of the sha256 of the spec ref as given. The digest keeps
// specs whose readable names coincide (x/y and x-y, case variants, or a spec
// named base) in separate repositories, so pruning one never touches another.
func layerTag(spec, hash string) string {
	ref := sha256.Sum256([]byte(spec))
	return "paddock/" + strings.ReplaceAll(strings.ToLower(spec), "/", "-") + "-" + hex.EncodeToString(ref[:4]) + ":" + hash[:12]
}

// ensureImages builds the base image and each plan layer that is not already
// present, and returns the tag of the last layer. Build output goes to
// stderr. Once the chain is built or reused, other tags of the chain's
// repositories are removed.
func (e *Engine) ensureImages(ctx context.Context, p *plan.Plan) (string, error) {
	hash, err := imageHash(images.BaseDockerfile, "", "")
	if err != nil {
		return "", err
	}
	tag := "paddock/base:" + hash[:12]
	if err := e.ensureImage(ctx, tag, func() error { return e.buildBase(ctx, tag) }); err != nil {
		return "", err
	}
	chain := []string{tag}
	for _, l := range p.Image.Layers {
		if l.Dockerfile == "" {
			continue
		}
		df, err := os.ReadFile(l.Dockerfile)
		if err != nil {
			return "", fmt.Errorf("image layer %s: %w", l.Spec, err)
		}
		dir := l.Context
		if dir == "" {
			dir = filepath.Dir(l.Dockerfile)
		}
		if dir, err = filepath.EvalSymlinks(dir); err != nil {
			return "", fmt.Errorf("image layer %s: %w", l.Spec, err)
		}
		hash, err := imageHash(df, dir, tag)
		if err != nil {
			return "", fmt.Errorf("image layer %s: %w", l.Spec, err)
		}
		next := layerTag(l.Spec, hash)
		parent := tag
		err = e.ensureImage(ctx, next, func() error {
			return e.build(ctx, next, l.Dockerfile, dir, parent)
		})
		if err != nil {
			return "", err
		}
		tag = next
		chain = append(chain, tag)
	}
	e.pruneSuperseded(ctx, chain)
	return tag, nil
}

// pruneSuperseded removes every tag of a repository in chain that is not in
// chain. Other repositories are never touched, and a failure to list or remove
// is logged and ignored: an image still used by a running container stays.
// Retention is not coordinated across processes: a concurrent run that
// prepared an older chain of the same spec and has not started its container
// yet loses its tag, and its `container run` fails.
func (e *Engine) pruneSuperseded(ctx context.Context, chain []string) {
	keep := make(map[string]bool, len(chain))
	repos := make(map[string]bool, len(chain))
	for _, t := range chain {
		keep[t] = true
		repo, _, _ := strings.Cut(t, ":")
		repos[repo] = true
	}
	out, err := e.cliOutput(ctx, "image", "ls", "--quiet")
	if err != nil {
		e.debugf("prune images: container image ls: %v", err)
		return
	}
	for _, line := range strings.Fields(string(out)) {
		name := strings.TrimPrefix(line, "docker.io/")
		repo, _, ok := strings.Cut(name, ":")
		if !ok || !repos[repo] || keep[name] {
			continue
		}
		if _, err := e.cliOutput(ctx, "image", "rm", line); err != nil {
			e.debugf("prune images: container image rm %s: %v", line, err)
			continue
		}
		e.debugf("removed superseded image %s", line)
	}
}

func (e *Engine) ensureImage(ctx context.Context, tag string, build func() error) error {
	have, err := e.imageExists(ctx, tag)
	if err != nil {
		return err
	}
	if have {
		return nil
	}
	e.debugf("building image %s", tag)
	return build()
}

func (e *Engine) imageExists(ctx context.Context, tag string) (bool, error) {
	out, err := e.cliOutput(ctx, "image", "ls", "--quiet")
	if err != nil {
		return false, fmt.Errorf("container image ls: %w", err)
	}
	for _, line := range strings.Fields(string(out)) {
		if strings.TrimPrefix(line, "docker.io/") == tag {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) buildBase(ctx context.Context, tag string) error {
	dir, err := os.MkdirTemp("", "paddock-base-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	df := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(df, images.BaseDockerfile, 0o644); err != nil {
		return err
	}
	return e.build(ctx, tag, df, dir, "")
}

// buildTimeout bounds one `container build`.
const buildTimeout = 30 * time.Minute

func (e *Engine) build(ctx context.Context, tag, dockerfile, contextDir, parent string) error {
	args := []string{"build", "-f", dockerfile, "-t", tag}
	if parent != "" {
		args = append(args, "--build-arg", "PADDOCK_PARENT="+parent)
	}
	args = append(args, contextDir)
	c, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, e.bin(), args...)
	cmd.Stdout = e.Stderr
	cmd.Stderr = e.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container build %s: %w", tag, err)
	}
	return nil
}
