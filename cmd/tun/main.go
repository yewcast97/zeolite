package main

import (
	"fmt"
	"io"
	"os"

	"zeolite/internal/sandbox"
)

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: tun")
		os.Exit(2)
	}
	device, err := openDevice()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tun:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, sandbox.TUNReady)
	if err := pump(device, os.Stdin, os.Stdout); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "tun: the Packet stream ended:", err)
	}
	// The device goes with the process.
}
