package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sdougbrown/paddock/images"
	"github.com/sdougbrown/paddock/internal/imgtag"
	"github.com/sdougbrown/paddock/internal/plan"
)

// ensureImages builds the base image and each plan layer that is not already
// present, and returns the tag of the last layer. Build output goes to
// stderr. Once the chain is built or reused, other tags of the chain's
// repositories are removed.
func (e *Engine) ensureImages(ctx context.Context, p *plan.Plan) (string, error) {
	hash, err := imgtag.Hash(images.BaseDockerfile, "", "")
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
		hash, err := imgtag.Hash(df, dir, tag)
		if err != nil {
			return "", fmt.Errorf("image layer %s: %w", l.Spec, err)
		}
		next := imgtag.LayerTag(l.Spec, hash)
		parent := tag
		err = e.ensureImage(ctx, next, func() error {
			return e.build(ctx, next, l.Dockerfile, dir, parent)
		})
		if err != nil {
			return "", err
		}
		tag = next
		chain = append(chain, next)
	}
	e.pruneSuperseded(ctx, chain)
	return tag, nil
}

// pruneSuperseded removes every tag of a repository in chain that is not in
// chain. Other repositories are never touched, and a failure to list or remove
// is logged and ignored: an image still used by a running container stays.
// Retention is not coordinated across processes: a concurrent run that
// prepared an older chain of the same spec and has not started its container
// yet loses its tag, and its `podman run` fails.
func (e *Engine) pruneSuperseded(ctx context.Context, chain []string) {
	keep := make(map[string]bool, len(chain))
	repos := make(map[string]bool, len(chain))
	for _, t := range chain {
		keep[t] = true
		keep["localhost/"+t] = true
		repo, _, _ := strings.Cut(t, ":")
		repos[repo] = true
	}
	out, err := e.cliOutput(ctx, "image", "ls", "--format", "json")
	if err != nil {
		e.debugf("prune images: podman image ls: %v", err)
		return
	}
	for _, name := range parseImageNames(out) {
		stripped := strings.TrimPrefix(strings.TrimPrefix(name, "localhost/"), "docker.io/")
		repo, _, ok := strings.Cut(stripped, ":")
		if !ok || !repos[repo] || keep[name] {
			continue
		}
		if _, err := e.cliOutput(ctx, "image", "rm", name); err != nil {
			e.debugf("prune images: podman image rm %s: %v", name, err)
			continue
		}
		e.debugf("removed superseded image %s", name)
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

// parseImageNames extracts every tag from `podman image ls --format json`.
// Unparsable output yields nil, which callers treat as "nothing present".
func parseImageNames(out []byte) []string {
	var list []struct {
		Names []string `json:"Names"`
	}
	if json.Unmarshal(out, &list) != nil {
		return nil
	}
	var names []string
	for _, img := range list {
		names = append(names, img.Names...)
	}
	return names
}

func (e *Engine) imageExists(ctx context.Context, tag string) (bool, error) {
	out, err := e.cliOutput(ctx, "image", "ls", "--format", "json")
	if err != nil {
		return false, fmt.Errorf("podman image ls: %w", err)
	}
	for _, name := range parseImageNames(out) {
		if name == tag || name == "localhost/"+tag || strings.TrimPrefix(name, "localhost/") == tag {
			return true, nil
		}
	}
	return false, nil
}

// buildBase builds the base image from a context holding only its
// Dockerfile, which is all the base tag's hash covers.
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

// buildTimeout bounds one `podman build`.
const buildTimeout = 30 * time.Minute

func (e *Engine) build(ctx context.Context, tag, dockerfile, contextDir, parent string) error {
	args := []string{"build", "-f", dockerfile, "-t", tag}
	if parent != "" {
		args = append(args, "--build-arg", "PADDOCK_PARENT="+parent)
	}
	args = append(args, contextDir)
	c, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	inv := e.invocation()
	cmd := exec.CommandContext(c, inv[0], append(inv[1:], args...)...)
	cmd.Stdout = e.Stderr
	cmd.Stderr = e.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("podman build %s: %w", tag, err)
	}
	return nil
}
