package applecontainer

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openTerminal returns the slave side of a new pseudo-terminal.
func openTerminal(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	fd := int(master.Fd())
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		t.Skipf("grant pseudo-terminal: %v", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		t.Skipf("unlock pseudo-terminal: %v", err)
	}
	var name [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		t.Skipf("name pseudo-terminal: %v", errno)
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	slave, err := os.OpenFile(string(name[:n]), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open pseudo-terminal peer: %v", err)
	}
	t.Cleanup(func() { slave.Close() })
	return slave
}
