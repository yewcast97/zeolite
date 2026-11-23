package hostwatch

import (
	"syscall"

	"zeolite/internal/probe"
)

// boundControl returns a probe's socket option that binds the socket to
// one interface of this Mac, by its index: its packets leave by that
// interface or not at all.
func boundControl(index int) probe.Control {
	return func(_, _ string, c syscall.RawConn) error {
		var bindErr error
		err := c.Control(func(fd uintptr) {
			bindErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, index)
		})
		if err != nil {
			return err
		}
		return bindErr
	}
}
