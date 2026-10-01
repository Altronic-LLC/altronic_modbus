//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd
// +build !darwin,!linux,!freebsd,!openbsd,!netbsd

package modbus

import (
	"github.com/goburrow/serial"
)

// No way to empty the receive buffer without waiting on this platform: the
// caller reads through the port instead.
func flushSerialPort(port serial.Port) (cnt int, ok bool) {
	return
}
