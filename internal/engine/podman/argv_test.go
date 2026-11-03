package podman

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/runstage"
)

func TestRunArgvGolden(t *testing.T) {
	tests := []struct {
		name string
		e    *Engine
		spec runSpec
		want []string
	}{
		{
			"plain",
			&Engine{},
			runSpec{Name: "paddock-abc", Image: "paddock/base:1234", Entrypoint: "/paddock-init", Workdir: "/ws"},
			[]string{"podman", "run", "--rm", "-i", "--init", "--name", "paddock-abc",
				"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW", "--security-opt", "label=disable", "--sig-proxy=false",
				"-w", "/ws", "--entrypoint", "/paddock-init", "paddock/base:1234"},
		},
		{
			"tty limits mounts env args",
			&Engine{},
			runSpec{Name: "paddock-abc", TTY: true, CPUs: 4, Memory: "4G",
				Mounts:     []runstage.GuestMount{{Host: "/h/ws", Guest: "/h/ws", ReadOnly: true}},
				Workdir:    "/h/ws",
				Env:        []string{"TERM=xterm"},
				Entrypoint: "/paddock-init", Image: "paddock/base:1234",
				Args: []string{"claude", "-p"}},
			[]string{"podman", "run", "--rm", "-i", "-t", "--init", "--name", "paddock-abc",
				"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW", "--security-opt", "label=disable", "--sig-proxy=false",
				"--cpus", "4", "-m", "4G",
				"-v", "/h/ws:/h/ws:ro", "-w", "/h/ws", "-e", "TERM=xterm",
				"--entrypoint", "/paddock-init", "paddock/base:1234", "claude", "-p"},
		},
		{
			"sudo prefix",
			&Engine{Sudo: true},
			runSpec{Name: "paddock-abc", Image: "paddock/base:1234", Entrypoint: "/paddock-init", Workdir: "/ws"},
			[]string{"sudo", "podman", "run", "--rm", "-i", "--init", "--name", "paddock-abc",
				"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW", "--security-opt", "label=disable", "--sig-proxy=false",
				"-w", "/ws", "--entrypoint", "/paddock-init", "paddock/base:1234"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.e.runArgv(tt.spec)
			if len(got) != len(tt.want) {
				t.Fatalf("runArgv = %q; want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("runArgv[%d] = %q, want %q (full: %q)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

func TestStopArgsAndTimeout(t *testing.T) {
	got := stopArgs("paddock-abc", 2)
	want := []string{"stop", "-t", "2", "paddock-abc"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stopArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestStopContainerBoundedByTimeout pins the stop's observable behavior: a
// podman stop that never completes errors when the stop timeout expires —
// stopTimeout(grace), cliTimeout plus the grace — rather than hanging.
func TestStopContainerBoundedByTimeout(t *testing.T) {
	bin, _ := fakePodman(t, "slow-stop")
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "slow-stop"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: bin}
	start := time.Now()
	if err := e.stopContainer("paddock-x", 2); err == nil {
		t.Fatal("a podman stop that never completes must error at the timeout")
	}
	if d := time.Since(start); d < stopTimeout(2)-time.Second {
		t.Errorf("stopContainer returned after %s; want ~%s", d, stopTimeout(2))
	}
}
