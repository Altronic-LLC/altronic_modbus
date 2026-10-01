//go:build darwin || linux || freebsd || openbsd || netbsd
// +build darwin linux freebsd openbsd netbsd

package modbus

import (
	"reflect"
	"syscall"

	"github.com/goburrow/serial"
)

// Empties the receive buffer of a serial port without waiting.
// Returns false if that could not be done, in which case nothing was read.
func flushSerialPort(port serial.Port) (cnt int, ok bool) {
	var fd int

	fd, ok = serialPortFd(port)
	if !ok || fd < 0 {
		ok = false
		return
	}

	cnt = drainFd(fd)

	return
}

// Returns the file descriptor of a goburrow/serial port.
// The package has no flush call and keeps the descriptor in an unexported
// field, hence the (read-only) reflection. Callers must cope with ok == false.
func serialPortFd(port serial.Port) (fd int, ok bool) {
	var v reflect.Value

	v = reflect.ValueOf(port)
	if v.Kind() != reflect.Ptr || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return
	}

	v = v.Elem().FieldByName("fd")
	if !v.IsValid() || v.Kind() != reflect.Int {
		return
	}

	fd = int(v.Int())
	ok = true

	return
}

// Reads a non-blocking descriptor until it has nothing left and returns the
// number of bytes read. goburrow/serial opens ports with O_NDELAY and never
// clears it, so read() returns EAGAIN instead of waiting once the buffer is
// empty.
func drainFd(fd int) (cnt int) {
	var rxbuf = make([]byte, 1024)
	var n int
	var err error

	for cnt < maxSerialFlush {
		n, err = syscall.Read(fd, rxbuf)
		if err == syscall.EINTR {
			continue
		}
		// EAGAIN (empty), EOF or a real error: nothing more to take
		if err != nil || n <= 0 {
			break
		}
		cnt += n
	}

	return
}
