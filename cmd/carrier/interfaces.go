//go:build darwin

package main

import (
	"net"
	"net/netip"
)

// seenInterface is one of this Mac's interfaces as the Carrier looks at
// it: the facts by which the Interface in use is chosen.
type seenInterface struct {
	Name  string
	Index int
	// Up is whether the interface is up.
	Up bool
	// Addrs are its IPv4 addresses.
	Addrs []netip.Addr
}

// lookAtInterfaces reads this Mac's interfaces.
func lookAtInterfaces() []seenInterface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make([]seenInterface, 0, len(all))
	for _, ifi := range all {
		s := seenInterface{Name: ifi.Name, Index: ifi.Index, Up: ifi.Flags&net.FlagUp != 0}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if addr, ok := netip.AddrFromSlice(ipn.IP.To4()); ok {
					s.Addrs = append(s.Addrs, addr)
				}
			}
		}
		seen = append(seen, s)
	}
	return seen
}

// chosen is an interface by its name and by the index it has now: an
// interface made again under its name is another.
type chosen struct {
	name  string
	index int
}

// choose returns the Interface in use among the Allowed Interfaces: the
// first that is there, is up and has an IPv4 address. It reads facts
// alone. Whether anything was ever carried by an interface, or a dial on
// it succeeded, changes nothing: a sandbox cannot steer itself onto a
// less preferred one.
func choose(allowed []string, seen []seenInterface) chosen {
	for _, name := range allowed {
		for _, s := range seen {
			if s.Name == name && s.Up && len(s.Addrs) > 0 {
				return chosen{name: s.Name, index: s.Index}
			}
		}
	}
	return chosen{}
}

// ownAddresses are the addresses this Mac has, on any interface.
func ownAddresses(seen []seenInterface) map[netip.Addr]bool {
	own := map[netip.Addr]bool{}
	for _, s := range seen {
		for _, a := range s.Addrs {
			own[a] = true
		}
	}
	return own
}
