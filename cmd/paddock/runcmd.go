package main

import (
	"fmt"
	"os"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine"
	"github.com/sdougbrown/paddock/internal/engine/applecontainer"
	"github.com/sdougbrown/paddock/internal/engine/bwrap"
	"github.com/sdougbrown/paddock/internal/engine/podman"
)

// newEngine returns the engine a plan names. It is a variable so tests can
// substitute a fake engine.
var newEngine = func(name string) (engine.Engine, error) {
	switch name {
	case config.EngineAppleContainer:
		return &applecontainer.Engine{Debug: os.Getenv("PADDOCK_DEBUG") == "1"}, nil
	case config.EnginePodman:
		return &podman.Engine{
			Debug: os.Getenv("PADDOCK_DEBUG") == "1",
			Sudo:  os.Getenv("PADDOCK_PODMAN_SUDO") == "1",
		}, nil
	case config.EngineBwrap:
		return &bwrap.Engine{Debug: os.Getenv("PADDOCK_DEBUG") == "1"}, nil
	}
	return nil, fmt.Errorf("unsupported engine %q", name)
}
