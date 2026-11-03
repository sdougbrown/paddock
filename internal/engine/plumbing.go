package engine

import (
	"fmt"
	"io"
	"os"
)

// Env is an engine's environment read: the engine's Getenv hook substitutes
// when set, the process environment otherwise.
func Env(getenv func(string) string, k string) string {
	if getenv != nil {
		return getenv(k)
	}
	return os.Getenv(k)
}

// LookupEnv is an engine's presence-sensitive environment read.
func LookupEnv(getenv func(string) string, k string) (string, bool) {
	if getenv != nil {
		v := getenv(k)
		return v, v != ""
	}
	return os.LookupEnv(k)
}

// Debugf writes an engine debug line to stderr: engines guard on their own
// debug flag and pass their stderr.
func Debugf(stderr io.Writer, format string, a ...any) {
	fmt.Fprintf(stderr, "paddock: "+format+"\n", a...)
}

// DefaultStreams points any standard stream the engine leaves unset at the
// process's own.
func DefaultStreams(stdin **os.File, stdout, stderr *io.Writer) {
	if *stdin == nil {
		*stdin = os.Stdin
	}
	if *stdout == nil {
		*stdout = os.Stdout
	}
	if *stderr == nil {
		*stderr = os.Stderr
	}
}
