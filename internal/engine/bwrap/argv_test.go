package bwrap

import (
	"strings"
	"testing"

	"github.com/sdougbrown/paddock/internal/runstage"
)

func TestRunArgvGolden(t *testing.T) {
	tests := []struct {
		name    string
		netNone bool
		mounts  []runstage.GuestMount
		want    string
	}{
		{"policy host keeps the network", false,
			[]runstage.GuestMount{{Host: "/h/ws", Guest: "/h/ws", ReadOnly: true}},
			"--ro-bind /h/ws:/h/ws"},
		{"policy none unshares the network", true, nil, "--unshare-net"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{}
			argv := e.runArgv(runSpec{
				NetNone: tt.netNone,
				Mounts:  tt.mounts,
				Workdir: "/h/ws",
				Argv:    []string{"claude", "-p"},
			}, "/run/home")
			joined := strings.Join(argv, " ")
			for _, want := range []string{
				"bwrap --die-with-parent", "--ro-bind / /", "--dev-bind /dev /dev",
				"--proc /proc", "--tmpfs /tmp", "--bind /run/home /run/home",
				"--chdir /h/ws", "claude -p",
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("argv %q lacks %q", joined, want)
				}
			}
			if tt.netNone && !strings.Contains(joined, "--unshare-net") {
				t.Errorf("policy none must unshare the network: %q", joined)
			}
			if !tt.netNone && strings.Contains(joined, "--unshare-net") {
				t.Errorf("policy host must keep the network: %q", joined)
			}
			if tt.mounts != nil {
				if !strings.Contains(joined, "--ro-bind /h/ws /h/ws") {
					t.Errorf("readonly mount must bind ro: %q", joined)
				}
			}
		})
	}
}
