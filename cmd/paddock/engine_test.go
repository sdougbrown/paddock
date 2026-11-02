package main

import (
	"os"
	"testing"

	"github.com/sdougbrown/paddock/internal/config"
	"github.com/sdougbrown/paddock/internal/engine/podman"
)

// TestNewEnginePodman checks the profile-to-engine wiring: the podman name
// resolves, and PADDOCK_PODMAN_SUDO reaches Engine.Sudo.
func TestNewEnginePodman(t *testing.T) {
	e, err := newEngine(config.EnginePodman)
	if err != nil {
		t.Fatalf("newEngine(podman): %v", err)
	}
	pe, ok := e.(*podman.Engine)
	if !ok {
		t.Fatalf("newEngine(podman) returned %T; want *podman.Engine", e)
	}
	if pe.Sudo {
		t.Error("Sudo set without PADDOCK_PODMAN_SUDO")
	}
	t.Setenv("PADDOCK_PODMAN_SUDO", "1")
	e, err = newEngine(config.EnginePodman)
	if err != nil {
		t.Fatalf("newEngine(podman): %v", err)
	}
	if pe, ok := e.(*podman.Engine); !ok || !pe.Sudo {
		t.Errorf("newEngine(podman) = %T with Sudo %v; want *podman.Engine with Sudo true", e, pe.Sudo)
	}
	if _, err := newEngine("nope"); err == nil {
		t.Error("newEngine(nope) succeeded; want an unsupported-engine error")
	}
	_ = os.Unsetenv("PADDOCK_PODMAN_SUDO")
}
