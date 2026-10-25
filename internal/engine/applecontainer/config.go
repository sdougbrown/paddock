package applecontainer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Engine runs plans with the `container` CLI. The zero value is usable.
type Engine struct {
	Bin       string // container CLI; default "container"
	Home      string // default: the user's home directory
	StateRoot string // default: <home>/.local/state/paddock
	CacheRoot string // default: resolve.CacheRoot(home)
	Stdin     *os.File
	Stdout    io.Writer
	Stderr    io.Writer
	Debug     bool
	Getenv    func(string) string
}

func (e *Engine) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	return "container"
}

func (e *Engine) debugf(format string, a ...any) {
	if e.Debug {
		fmt.Fprintf(e.Stderr, "paddock: "+format+"\n", a...)
	}
}

// cliTimeout bounds each short `container` CLI call.
const cliTimeout = 5 * time.Second

// cliOutput runs one short `container` CLI call under ctx, bounded by
// cliTimeout, and returns its standard output.
func (e *Engine) cliOutput(ctx context.Context, args ...string) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	return exec.CommandContext(c, e.bin(), args...).Output()
}
