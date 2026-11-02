package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/sdougbrown/paddock/images"
	"github.com/sdougbrown/paddock/internal/imgtag"
	"github.com/sdougbrown/paddock/internal/plan"
)

// ensureImages builds the base image and each plan layer that is not already
// present, and returns the tag of the last layer. Build output goes to
// stderr. The chain order, hashing, and pruning live in imgtag; the calls
// below are the podman CLI's shape.
func (e *Engine) ensureImages(ctx context.Context, p *plan.Plan) (string, error) {
	return imgtag.EnsureChain(ctx, images.BaseDockerfile, p.Image.Layers, imgtag.ChainOps{
		Build:       e.build,
		ImageExists: e.imageExists,
		Prune:       e.pruneSuperseded,
		Debugf:      e.debugf,
	})
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
