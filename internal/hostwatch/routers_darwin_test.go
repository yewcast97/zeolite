package hostwatch

import (
	"net/netip"
	"syscall"
	"testing"

	"golang.org/x/net/route"
)

// defaultRoute is a default route as the routing table lists one: by an
// interface, with its flags, through a router or to the link itself.
func defaultRoute(index, flags int, gateway route.Addr) *route.RouteMessage {
	addrs := make([]route.Addr, syscall.RTAX_MAX)
	addrs[syscall.RTAX_DST] = &route.Inet4Addr{}
	addrs[syscall.RTAX_GATEWAY] = gateway
	addrs[syscall.RTAX_NETMASK] = &route.Inet4Addr{}
	return &route.RouteMessage{Index: index, Flags: flags, Addrs: addrs}
}

// Of the default routes of a routing table, those through a router say
// which interfaces have a way out of their own. The routes are a Mac's
// with Wi-Fi, a VM's host-only bridge and a tunnel: `en0` has
// `default 192.168.1.1 UGScg`, the other two `default link#N UCSIg`.
func TestRoutersOfARoutingTable(t *testing.T) {
	const (
		up      = syscall.RTF_UP
		gateway = syscall.RTF_GATEWAY
		static  = syscall.RTF_STATIC
		cloning = syscall.RTF_CLONING
		scoped  = syscall.RTF_IFSCOPE
	)
	router := &route.Inet4Addr{IP: [4]byte{192, 168, 1, 1}}
	second := &route.Inet4Addr{IP: [4]byte{10, 0, 0, 1}}
	notDefault := defaultRoute(40, up|gateway|static, router)
	notDefault.Addrs[syscall.RTAX_DST] = &route.Inet4Addr{IP: [4]byte{10, 8, 0, 0}}
	halfDefault := defaultRoute(41, up|gateway|static, router)
	halfDefault.Addrs[syscall.RTAX_NETMASK] = &route.Inet4Addr{IP: [4]byte{128, 0, 0, 0}}
	messages := []route.Message{
		defaultRoute(11, up|gateway|static, router),
		// A second way out, whose default route is scoped to it.
		defaultRoute(14, up|gateway|static|scoped, second),
		// A host-only bridge and a tunnel: to the link, through no router.
		defaultRoute(26, up|cloning|static|scoped, &route.LinkAddr{Index: 26}),
		defaultRoute(30, up|cloning|static|scoped, &route.LinkAddr{Index: 30}),
		// The flag without a router's address, and a route that is down.
		defaultRoute(31, up|gateway|static, &route.LinkAddr{Index: 31}),
		defaultRoute(32, gateway|static, router),
		// Routes through a router that are not the default route: to a
		// network, and to half of all addresses, as a tunnel adds them.
		notDefault,
		halfDefault,
		&route.InterfaceMessage{Index: 11},
	}
	got := routersOf(messages)
	want := map[int]netip.Addr{11: netip.MustParseAddr("192.168.1.1"), 14: netip.MustParseAddr("10.0.0.1")}
	if len(got) != len(want) {
		t.Fatalf("the routers are %v, want %v", got, want)
	}
	for index, router := range want {
		if got[index] != router {
			t.Errorf("the router of interface %d is %v, want %v", index, got[index], router)
		}
	}
}

// The real routing table of this Mac is read without a program, and a
// router it names is an IPv4 address.
func TestThisMacsRoutingTableIsRead(t *testing.T) {
	routers, err := defaultRouters()
	if err != nil {
		t.Fatal(err)
	}
	for index, router := range routers {
		if index <= 0 || !router.Is4() || router.IsUnspecified() {
			t.Errorf("interface %d has the router %v", index, router)
		}
	}
}
