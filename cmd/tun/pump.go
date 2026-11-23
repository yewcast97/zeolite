// Command tun makes a sandbox's TUN device in the network namespace it
// is run in, and moves packets between the device and its standard
// streams as the Packet stream: each packet behind its length.
//
//	tun
//
// The app runs it in a gatekeeper's container, whose namespace the
// workstation shares. It opens no connection and listens on nothing:
// what it reads from the device it writes to its standard output, and
// what it reads from its standard input it writes to the device. When
// either stream ends it exits, and the device goes with it: nothing of
// the sandbox can leave.
package main

import (
	"bufio"
	"io"

	"zeolite/internal/sandbox"
)

// pump moves packets between a TUN device and a Packet stream until
// either ends, and returns why it stopped.
func pump(device io.ReadWriter, fromStream io.Reader, toStream io.Writer) error {
	ended := make(chan error, 3)
	// Device to stream. Packets that are waiting go out in one write.
	packets := make(chan []byte, 512)
	go func() {
		for {
			packet := make([]byte, sandbox.MaxPacket)
			n, err := device.Read(packet)
			if err != nil {
				ended <- err
				return
			}
			packets <- packet[:n]
		}
	}()
	go func() {
		w := bufio.NewWriterSize(toStream, 1<<16)
		var frame []byte
		for packet := range packets {
			var err error
			if frame, err = sandbox.AppendPacket(frame[:0], packet); err != nil {
				continue // longer than the device's packets are: not one of them
			}
			if _, err := w.Write(frame); err != nil {
				ended <- err
				return
			}
			if len(packets) == 0 {
				if err := w.Flush(); err != nil {
					ended <- err
					return
				}
			}
		}
	}()
	// Stream to device.
	go func() {
		r := bufio.NewReaderSize(fromStream, 1<<16)
		packet := make([]byte, sandbox.MaxPacket)
		for {
			n, err := sandbox.ReadPacket(r, packet)
			if err != nil {
				ended <- err
				return
			}
			device.Write(packet[:n]) // a packet the kernel refuses is a lost packet
		}
	}()
	return <-ended
}
