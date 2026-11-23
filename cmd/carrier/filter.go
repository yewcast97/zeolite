//go:build darwin

package main

import (
	"encoding/binary"
	"net/netip"

	"zeolite/internal/sandbox"
)

// carried reports whether a packet off the Packet stream is one the
// Carrier's network stack is given to read: a whole IPv4 packet from the
// sandbox's own address that is TCP, or a name query to the sandbox's
// resolver. Everything else is dropped before the stack sees it: other
// protocols, IPv6, fragments, and anything that claims another source.
func carried(packet []byte, resolver netip.Addr) bool {
	const minHeader = 20
	if len(packet) < minHeader || packet[0]>>4 != 4 {
		return false
	}
	header := int(packet[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(packet[2:4]))
	if header < minHeader || header > len(packet) || total < header || total > len(packet) {
		return false
	}
	// A fragment: more follow, or this one is not the first.
	if binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		return false
	}
	if netip.AddrFrom4([4]byte(packet[12:16])) != sandbox.SandboxAddress {
		return false
	}
	switch packet[9] {
	case 6: // TCP
		return true
	case 17: // UDP
		if total < header+8 {
			return false
		}
		port := binary.BigEndian.Uint16(packet[header+2 : header+4])
		return port == 53 && netip.AddrFrom4([4]byte(packet[16:20])) == resolver
	}
	return false
}
