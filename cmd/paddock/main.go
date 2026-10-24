// Command paddock runs one command in a sandbox with explicit mounts, limits,
// and network policy. Invoked as paddock-init it is the guest entrypoint.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const version = "0.1.0-dev"

func main() {
	if filepath.Base(os.Args[0]) == "paddock-init" {
		fmt.Fprintln(os.Stderr, "paddock-init: not implemented")
		os.Exit(2)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: paddock version")
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "paddock", version)
		return 0
	}
	fmt.Fprintf(stderr, "paddock: unknown command %q\n", args[0])
	return 2
}
