package podman

import (
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
				"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW", "--security-opt", "label=disable",
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
				"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW", "--security-opt", "label=disable",
				"--cpus", "4", "-m", "4G",
				"-v", "/h/ws:/h/ws:ro", "-w", "/h/ws", "-e", "TERM=xterm",
				"--entrypoint", "/paddock-init", "paddock/base:1234", "claude", "-p"},
		},
		{
			"sudo prefix",
			&Engine{Sudo: true},
			runSpec{Name: "paddock-abc", Image: "paddock/base:1234", Entrypoint: "/paddock-init", Workdir: "/ws"},
			[]string{"sudo", "podman", "run", "--rm", "-i", "--init", "--name", "paddock-abc",
				"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW", "--security-opt", "label=disable",
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
	if d := stopTimeout(2); d != cliTimeout+2*time.Second {
		t.Errorf("stopTimeout(2) = %s, want %s", d, cliTimeout+2*time.Second)
	}
}
