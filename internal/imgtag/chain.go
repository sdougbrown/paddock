package imgtag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sdougbrown/paddock/internal/plan"
)

// ChainOps carries the runtime-specific parts of building a spec chain:
// building one layer, reporting whether a tag exists, and pruning
// superseded tags of the chain's repositories.
type ChainOps struct {
	// Build builds the image tag from the Dockerfile and context, with
	// parent as its base when parent is not empty.
	Build func(ctx context.Context, tag, dockerfile, contextDir, parent string) error
	// ImageExists reports whether tag is in local storage.
	ImageExists func(ctx context.Context, tag string) (bool, error)
	// Prune removes every tag of a chain repository that is not in chain.
	Prune func(ctx context.Context, chain []string)
	// Debugf is optional; a non-nil one receives progress notes.
	Debugf func(format string, args ...any)
}

func (o ChainOps) debugf(format string, a ...any) {
	if o.Debugf != nil {
		o.Debugf(format, a...)
	}
}

// EnsureChain builds the base image and each plan layer that is not already
// present, and returns the tag of the last layer. The base tag hashes the
// base Dockerfile alone; each layer tag hashes its Dockerfile, its context
// tree, and the parent tag. Once the chain is built or reused, Prune runs
// with the chain.
func EnsureChain(ctx context.Context, baseDockerfile []byte, layers []plan.ImageLayer, ops ChainOps) (string, error) {
	hash, err := Hash(baseDockerfile, "", "")
	if err != nil {
		return "", err
	}
	tag := "paddock/base:" + hash[:12]
	if err := ops.ensureImage(ctx, tag, func() error {
		return ops.buildBase(ctx, tag, baseDockerfile)
	}); err != nil {
		return "", err
	}
	chain := []string{tag}
	for _, l := range layers {
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
		hash, err := Hash(df, dir, tag)
		if err != nil {
			return "", fmt.Errorf("image layer %s: %w", l.Spec, err)
		}
		next := LayerTag(l.Spec, hash)
		parent := tag
		if err := ops.ensureImage(ctx, next, func() error {
			return ops.Build(ctx, next, l.Dockerfile, dir, parent)
		}); err != nil {
			return "", err
		}
		tag = next
		chain = append(chain, tag)
	}
	if ops.Prune != nil {
		ops.Prune(ctx, chain)
	}
	return tag, nil
}

func (o ChainOps) ensureImage(ctx context.Context, tag string, build func() error) error {
	have, err := o.ImageExists(ctx, tag)
	if err != nil {
		return err
	}
	if have {
		return nil
	}
	o.debugf("building image %s", tag)
	return build()
}

func (o ChainOps) buildBase(ctx context.Context, tag string, baseDockerfile []byte) error {
	dir, err := os.MkdirTemp("", "paddock-base-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	df := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(df, baseDockerfile, 0o644); err != nil {
		return err
	}
	return o.Build(ctx, tag, df, dir, "")
}
