package sandbox

import "net/netip"

// NonPublicPrefixes are the address ranges a sandbox's egress never
// reaches: loopback, private networks, link-local, carrier-grade NAT,
// multicast, reserved ranges, and the IPv6 ranges that embed an IPv4
// address. The Mac's own services, the LAN, the VM and other containers
// all live in them.
var NonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),

	netip.MustParsePrefix("::/127"),       // unspecified and loopback
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64
	netip.MustParsePrefix("2002::/16"),    // 6to4
	netip.MustParsePrefix("fc00::/7"),     // unique local
	netip.MustParsePrefix("fe80::/10"),    // link-local
	netip.MustParsePrefix("ff00::/8"),     // multicast
}

// StandInAddresses is the range of the stand-in addresses. What an
// Interface leads to may answer a name with one of them in place of the
// name's own, and take a connection made to it for one to the name. The
// range is kept for measuring networks, so no host of the internet has
// an address in it, and it is among the NonPublicPrefixes: a sandbox
// connects to it only by an Allowed Interface that is said to answer
// names so, and while that is the Interface in use.
var StandInAddresses = netip.MustParsePrefix("198.18.0.0/15")

// StandInAddress reports whether a is a stand-in address.
func StandInAddress(a netip.Addr) bool {
	return a.IsValid() && StandInAddresses.Contains(a.Unmap().WithZone(""))
}

// PublicAddress reports whether a is an address on the public internet.
// An IPv4-mapped IPv6 address is judged as the IPv4 address it holds.
func PublicAddress(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap().WithZone("")
	for _, p := range NonPublicPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
