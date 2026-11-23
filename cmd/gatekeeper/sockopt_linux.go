package main

import (
	"errors"
	"os"
	"sync/atomic"
	"syscall"

	"zeolite/internal/sandbox"
)

// nextSourcePort counts the probe's sockets: each takes the next of the
// Probe Source Ports.
var nextSourcePort atomic.Uint32

// probeControl returns a net.Dialer.Control that makes a socket a
// probe's: it marks it, so that the ruleset knows it is the
// gatekeeper's, and binds it to one of the Probe Source Ports, so that
// the Carrier knows it too. The ports are taken in turn, and one comes
// round again long after the connection that had it is forgotten; a
// port that is still taken is passed over.
func probeControl() func(network, address string, c syscall.RawConn) error {
	const ports = sandbox.ProbeSourcePortLast - sandbox.ProbeSourcePortFirst + 1
	return func(_, _ string, c syscall.RawConn) error {
		var sockErr error
		err := c.Control(func(fd uintptr) {
			if sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, markProbe); sockErr != nil {
				return
			}
			for range 16 {
				port := sandbox.ProbeSourcePortFirst + int(nextSourcePort.Add(1)%ports)
				sockErr = syscall.Bind(int(fd), &syscall.SockaddrInet4{Port: port})
				if !errors.Is(sockErr, syscall.EADDRINUSE) {
					return
				}
			}
		})
		if err != nil {
			return err
		}
		return sockErr
	}
}

// localPortRange is where the namespace says which ports it hands out
// by itself.
const localPortRange = "/proc/sys/net/ipv4/ip_local_port_range"

// sourcePortsFree checks that the namespace hands out none of the Probe
// Source Ports by itself: a connection of the sandbox's that was given
// one would be rejected.
func sourcePortsFree() error {
	data, err := os.ReadFile(localPortRange)
	if err != nil {
		return err
	}
	return checkLocalPortRange(string(data))
}
