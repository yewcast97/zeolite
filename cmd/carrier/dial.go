//go:build darwin

package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"time"

	"zeolite/internal/sandbox"
)

// Why the Carrier makes no socket for a destination. Neither is an
// answer from the far end, and the sandbox is told neither: it hears
// nothing.
var (
	errNoInterface = errors.New("there is no Interface in use")
	errNotCarried  = errors.New("the Carrier carries nothing there")
)

// dialTimeout is how long a connection may take to be made.
const dialTimeout = 10 * time.Second

// connect is the one place where the Carrier makes a socket. The socket
// is bound to the Interface in use, and goes to a public address, or to
// a stand-in address where that Interface answers names with those,
// that is neither this Mac's own nor one the sandbox is expected to be
// seen at. The destination is an address: no name is ever resolved.
func (c *carrier) connect(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error) {
	c.mu.Lock()
	use, own := c.inUse, c.own[dst.Addr()]
	c.mu.Unlock()
	if use == (chosen{}) {
		return nil, errNoInterface
	}
	if !dst.Addr().Is4() || !c.carriesTo(dst.Addr(), use) || own || c.exits[dst.Addr()] {
		return nil, errNotCarried
	}
	return c.dialBound(ctx, use.index, network, dst.String())
}

// carriesTo is whether the Carrier carries to an address by an
// Interface in use: to a public address by any, and to a stand-in
// address by one where names are answered with those. Where a stand-in
// address leads is decided where that Interface leads, by the name it
// stands for: the Carrier does not know it, and by no other Interface
// does the address mean anything.
func (c *carrier) carriesTo(a netip.Addr, use chosen) bool {
	return sandbox.PublicAddress(a) || c.standIns[use.name] && sandbox.StandInAddress(a)
}

// dialBound makes a socket bound to the interface of an index, with
// IP_BOUND_IF: its packets leave by that interface or not at all.
func dialBound(ctx context.Context, index int, network, address string) (net.Conn, error) {
	dialer := net.Dialer{
		Timeout: dialTimeout,
		Control: func(_, _ string, c syscall.RawConn) error {
			var bindErr error
			if err := c.Control(func(fd uintptr) {
				bindErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, index)
			}); err != nil {
				return err
			}
			return bindErr
		},
	}
	return dialer.DialContext(ctx, network, address)
}
