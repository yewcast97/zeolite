//go:build !linux

package main

import (
	"errors"
	"os"
)

// openDevice makes the TUN device: on Linux only, where sandboxes run.
func openDevice() (*os.File, error) {
	return nil, errors.New("a sandbox's TUN device is made on Linux only")
}
