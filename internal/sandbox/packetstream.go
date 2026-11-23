package sandbox

import (
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
)

// A sandbox's packets cross between its TUN device, in its network
// namespace, and its Carrier, on the Mac, as the Packet stream: each
// packet behind its length, a 16-bit number, most significant byte
// first. Both ends use the one codec here.

// The TUN device of a sandbox: its name, its address, which is the same
// in every sandbox and is the sandbox's own, and the largest packet it
// carries.
const (
	TUNDevice = "tun0"
	PacketMTU = 1500
)

// TUNReady is the line tun writes on standard error once the device is
// there. Its standard output carries packets and nothing else.
const TUNReady = "tun: ready"

// SandboxAddress is the address every sandbox has on its TUN device. It
// is not a public address: nothing on the internet is at it.
var SandboxAddress = netip.MustParseAddr("192.0.0.2")

// MaxPacket is the longest packet a Packet stream carries. A length
// beyond it is not a packet's: the stream is broken.
const MaxPacket = PacketMTU + 64

// ErrPacketTooLong says that a Packet stream announced a packet longer
// than any is.
var ErrPacketTooLong = errors.New("the Packet stream holds a packet longer than any is")

// AppendPacket appends one packet, behind its length, to a buffer that is
// then written to a Packet stream in one piece.
func AppendPacket(buf, packet []byte) ([]byte, error) {
	if len(packet) > MaxPacket {
		return buf, ErrPacketTooLong
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(packet)))
	return append(buf, packet...), nil
}

// ReadPacket reads the next packet of a Packet stream into buf, which
// holds at least MaxPacket bytes, and returns its length.
func ReadPacket(r io.Reader, buf []byte) (int, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(header[:]))
	if n > MaxPacket || n > len(buf) {
		return 0, ErrPacketTooLong
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	return n, nil
}
