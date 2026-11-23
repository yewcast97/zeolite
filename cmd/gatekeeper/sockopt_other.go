//go:build !linux

package main

import (
	"errors"
	"syscall"
)

// The gatekeeper runs only in a Linux container. This stands in for its
// socket option elsewhere, so that the rest of the package compiles and
// its tests run on the Mac.

var errLinuxOnly = errors.New("the gatekeeper runs only on Linux")

func probeControl() func(network, address string, c syscall.RawConn) error {
	return func(string, string, syscall.RawConn) error { return errLinuxOnly }
}

func sourcePortsFree() error { return errLinuxOnly }
