//go:build darwin || linux || freebsd || openbsd || netbsd
// +build darwin linux freebsd openbsd netbsd

package modbus

import (
	"syscall"
	"testing"
	"time"

	"github.com/goburrow/serial"
)

// The non-blocking flush reads the port's descriptor out of goburrow/serial's
// port object: make sure the field is where we expect it for the version in
// go.mod (an unopened port holds -1).
func TestSerialPortFdIsReachable(t *testing.T) {
	var fd int
	var ok bool

	fd, ok = serialPortFd(serial.New())
	if !ok {
		t.Fatalf("could not find the file descriptor of a goburrow/serial port")
	}
	if fd != -1 {
		t.Errorf("expected fd -1 on an unopened port, got %v", fd)
	}

	_, ok = serialPortFd(nil)
	if ok {
		t.Errorf("a nil port has no file descriptor")
	}
}

func TestDrainFd(t *testing.T) {
	var fds [2]int
	var err error
	var n int
	var start time.Time

	fds, err = syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	// the serial port's descriptor is non-blocking
	err = syscall.SetNonblock(fds[0], true)
	if err != nil {
		t.Fatalf("failed to make the descriptor non-blocking: %v", err)
	}

	// nothing pending: must return at once
	start = time.Now()
	n = drainFd(fds[0])
	if n != 0 {
		t.Errorf("expected 0 bytes drained, got %v", n)
	}
	if time.Since(start) > 5*time.Millisecond {
		t.Errorf("draining an empty descriptor took %v", time.Since(start))
	}

	// more than one read's worth of stale bytes
	_, err = syscall.Write(fds[1], make([]byte, 1500))
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	n = drainFd(fds[0])
	if n != 1500 {
		t.Errorf("expected 1500 bytes drained, got %v", n)
	}
	n = drainFd(fds[0])
	if n != 0 {
		t.Errorf("expected nothing left, got %v", n)
	}
}
