package hostwatch

import (
	"net/netip"
	"syscall"

	"golang.org/x/net/route"
)

// defaultRouters reads this Mac's routing table, and returns, for each
// interface that has a default route through a router, that router, by
// the interface's index. No program is run: the table is read from the
// kernel.
func defaultRouters() (map[int]netip.Addr, error) {
	rib, err := route.FetchRIB(syscall.AF_INET, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, err
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, err
	}
	return routersOf(messages), nil
}

// routersOf picks the default routes that go through a router out of a
// routing table. A default route that names no router, as a host-only
// bridge's and a tunnel's do, is no physical Interface's: a socket
// bound to such an interface still connects without an error, which is
// why the table is asked and not the socket.
func routersOf(messages []route.Message) map[int]netip.Addr {
	routers := map[int]netip.Addr{}
	for _, m := range messages {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Flags&syscall.RTF_UP == 0 || rm.Flags&syscall.RTF_GATEWAY == 0 {
			continue
		}
		if len(rm.Addrs) <= syscall.RTAX_NETMASK {
			continue
		}
		// The default route: to 0.0.0.0, with no bit of it fixed.
		if dst, ok := rm.Addrs[syscall.RTAX_DST].(*route.Inet4Addr); !ok || dst.IP != [4]byte{} {
			continue
		}
		if mask, ok := rm.Addrs[syscall.RTAX_NETMASK].(*route.Inet4Addr); rm.Addrs[syscall.RTAX_NETMASK] != nil && (!ok || mask.IP != [4]byte{}) {
			continue
		}
		gateway, ok := rm.Addrs[syscall.RTAX_GATEWAY].(*route.Inet4Addr)
		if !ok {
			continue
		}
		routers[rm.Index] = netip.AddrFrom4(gateway.IP)
	}
	return routers
}
