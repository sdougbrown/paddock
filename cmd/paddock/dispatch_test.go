package main

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/sdougbrown/paddock/internal/engine/applecontainer"
)

// TestMain runs main itself when PADDOCK_TEST_MAIN is set, so a test can
// re-execute this binary as the paddock command.
func TestMain(m *testing.M) {
	if os.Getenv("PADDOCK_TEST_MAIN") == "1" {
		main()
	}
	os.Exit(m.Run())
}

// TestMainDispatchesWatchdog runs main as Engine.Run would start the
// watchdog: WatchdogCommand first, the parent's pipe on fd 3, and the ready
// pipe on fd 4. main must hand it to the watchdog, which signals ready and
// exits 0 once told the run ended normally.
func TestMainDispatchesWatchdog(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	// A CLI path that does not exist, so a watchdog that misread the done
	// byte cannot reach a real container.
	cmd := exec.Command(os.Args[0], applecontainer.WatchdogCommand, "/nonexistent/container", "paddock-test")
	cmd.Env = append(os.Environ(), "PADDOCK_TEST_MAIN=1")
	cmd.ExtraFiles = []*os.File{r, readyW}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-exited
	})
	r.Close()
	readyW.Close()
	readyR.SetReadDeadline(time.Now().Add(10 * time.Second))
	var b [1]byte
	if n, err := readyR.Read(b[:]); n != 1 {
		t.Fatalf("no ready byte from main: %v", err)
	}
	// 'x' is the watchdog's done byte (applecontainer.watchdogDone).
	if _, err := w.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	select {
	case err := <-exited:
		exited <- err
		if err != nil {
			t.Fatalf("watchdog exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog still running after 10s")
	}
}
