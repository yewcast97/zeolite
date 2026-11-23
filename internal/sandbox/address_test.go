package sandbox

import (
	"net/netip"
	"testing"
)

func TestPublicAddress(t *testing.T) {
	public := []string{"1.1.1.1", "203.0.113.7", "172.32.0.1", "100.128.0.1", "2606:4700:4700::1111"}
	nonPublic := []string{
		"127.0.0.1", "127.1.2.3", "10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.5.2",
		"169.254.169.254", "100.64.0.1", "198.18.0.1", "0.0.0.0", "224.0.0.251", "255.255.255.255",
		"::1", "::", "fe80::1", "fd00::1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::7f00:1", "2002:7f00:1::",
	}
	for _, s := range public {
		if !PublicAddress(netip.MustParseAddr(s)) {
			t.Errorf("%s is judged non-public", s)
		}
	}
	for _, s := range nonPublic {
		if PublicAddress(netip.MustParseAddr(s)) {
			t.Errorf("%s is judged public", s)
		}
	}
	if PublicAddress(netip.Addr{}) {
		t.Error("the zero address is judged public")
	}
}

// The stand-in addresses are one range, which is none of the public
// internet's: whatever carries them carries nothing else that is not
// public.
func TestStandInAddress(t *testing.T) {
	for _, s := range []string{"198.18.0.0", "198.18.0.13", "198.19.255.255", "::ffff:198.18.0.13"} {
		if a := netip.MustParseAddr(s); !StandInAddress(a) || PublicAddress(a) {
			t.Errorf("%s: a stand-in address %v, public %v: want a stand-in address that is not public", s, StandInAddress(a), PublicAddress(a))
		}
	}
	for _, s := range []string{"198.17.255.255", "198.20.0.0", "10.0.0.5", "192.168.5.2", "127.0.0.1", "100.64.0.1", "169.254.169.254", "1.1.1.1", "::1"} {
		if StandInAddress(netip.MustParseAddr(s)) {
			t.Errorf("%s is judged a stand-in address", s)
		}
	}
	if StandInAddress(netip.Addr{}) {
		t.Error("the zero address is judged a stand-in address")
	}
	within := false
	for _, p := range NonPublicPrefixes {
		within = within || p == StandInAddresses
	}
	if !within {
		t.Errorf("%s is not among the ranges a sandbox's egress never reaches by itself", StandInAddresses)
	}
}
