//go:build integration

package integration

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"zeolite/internal/hostwatch"
	"zeolite/internal/orchestrator"
	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
)

// The tests here need a way out that answers names with stand-in
// addresses: a tunnel that another app of this Mac's user keeps up, by
// which what is bound to no Interface leaves this Mac. They skip where
// this Mac's names are answered with their own addresses.

// standInName is a name whose host answers on port 80: the package host
// a workstation's image is built from.
const standInName = "deb.debian.org"

// waysWithStandIns returns the tunnel by which a socket bound to no
// Interface leaves this Mac, where names are answered with stand-in
// addresses there, and a port of this Mac's beside it, as Wi-Fi is. It
// skips the test where there is no such tunnel.
func waysWithStandIns(t *testing.T) (tunnel, port string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", standInName)
	if err != nil {
		t.Skipf("this Mac does not resolve %s: %v", standInName, err)
	}
	answered := false
	for _, a := range addrs {
		answered = answered || sandbox.StandInAddress(a)
	}
	if !answered {
		t.Skipf("this Mac is given %s at %v: no way out here answers names with stand-in addresses", standInName, addrs)
	}
	// The interface a socket bound to none leaves by: here, the tunnel.
	tunnel, err = physicalInterface()
	if err != nil {
		t.Skipf("the tunnel could not be found: %v", err)
	}
	// A way out that has a hardware address and leads through a router:
	// Wi-Fi, Ethernet.
	for _, ifc := range hostwatch.Interfaces() {
		if ifi, err := net.InterfaceByName(ifc.Name); err == nil && ifc.Name != tunnel && len(ifi.HardwareAddr) > 0 && ifi.Flags&net.FlagPointToPoint == 0 {
			return tunnel, ifc.Name
		}
	}
	t.Skip("this Mac has no port of its own beside the tunnel")
	return "", ""
}

// By a tunnel that answers names with stand-in addresses, a sandbox
// whose Setup says so is given a name at a stand-in address, connects
// to it and is answered, and its gate opens: the gatekeeper's probes
// are answered there too. One whose Setup does not say so opens no
// gate, as before: no probe of its connects to what its probe URLs'
// names are answered with.
func TestAStandInAddressAnswersByTheTunnelThatHandsItOut(t *testing.T) {
	tunnel, _ := waysWithStandIns(t)
	said := newConfig("standin-tunnel", "By a tunnel, stand-in addresses said")
	said.AllowedInterfaces = []sandbox.AllowedInterface{{Name: tunnel, AnyExitAddress: true, StandInAddresses: true}}
	unsaid := newConfig("standin-tunnel-unsaid", "By a tunnel, nothing said")
	unsaid.AllowedInterfaces = []sandbox.AllowedInterface{{Name: tunnel, AnyExitAddress: true}}
	began := time.Now()
	start(t, said, nil, nil)
	start(t, unsaid, nil, nil)

	status := waitState(t, said.ID, sandbox.StateGateOpen, 40*time.Second)
	if _, err := sandbox.ParsePublicIPv4(status.ExitAddress); err != nil {
		t.Errorf("the gate is open on %q, which is no exit address", status.ExitAddress)
	}
	out, _ := inSandbox(said.ID, "getent ahostsv4 "+standInName+" | head -1 | cut -d' ' -f1")
	found, err := netip.ParseAddr(trimmed(out))
	if err != nil || !sandbox.StandInAddress(found) {
		t.Fatalf("in the sandbox %s is at %q: the tunnel was to answer with a stand-in address", standInName, trimmed(out))
	}
	capture := captureTUN(t, said.ID)
	if got := attempt(t, said.ID, standInName, 80); got != answered {
		t.Errorf("%s, at the stand-in address %s, was %s, want %s", standInName, found, got, answered)
	}
	sent := 0
	for _, p := range capture.packets(t) {
		switch {
		case sandbox.StandInAddress(p.dst) && p.tcp():
			sent++
		case !sandbox.PublicAddress(p.dst):
			t.Errorf("on the device for an address that is neither public nor a stand-in address: %s", p.line)
		}
	}
	if sent == 0 {
		t.Error("nothing for a stand-in address was on the sandbox's device")
	}
	// What is not public and no stand-in address is refused as ever.
	for what, host := range map[string]string{
		"a private address":                   "10.0.0.1",
		"the Mac, by the VM's address for it": hostGateway(t).String(),
		"the tunnel's own address":            "198.18.0.1",
	} {
		if got := attempt(t, said.ID, host, 80); got == answered || got == closed {
			t.Errorf("%s (%s) was %s", what, host, got)
		}
	}

	// The other sandbox has had as long, and more.
	time.Sleep(time.Until(began.Add(45 * time.Second)))
	for _, s := range updates.since(unsaid.ID, began) {
		if s.State == sandbox.StateGateOpen {
			t.Fatalf("a sandbox whose Setup does not say that names are answered with stand-in addresses opened its gate by the tunnel: %+v", s)
		}
	}
	if got := updates.statusOf(unsaid.ID); got.State != sandbox.StateGateLocked || got.LockReason != sandbox.LockProbeUnanswered {
		t.Errorf("that sandbox is %s, want its gate locked for an unanswered probe", got)
	}
}

// While a tunnel answers this Mac's names with stand-in addresses, a
// workstation's image is built by a port of this Mac's all the same:
// the step asks the sandbox's resolver by that port, is given the
// name's own address, and reaches it.
func TestAnImageIsBuiltByAPortBesideATunnelThatAnswersWithStandInAddresses(t *testing.T) {
	_, port := waysWithStandIns(t)
	cfg := newConfig("build-beside-tunnel", "Built by a port, beside a tunnel")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: port, AnyExitAddress: true}}
	m, nonce := newPack("test-built-beside-a-tunnel", strings.Join([]string{
		"mkdir /built && echo $nonce > /built/nonce",
		"getent ahostsv4 " + standInName + " | head -1 | cut -d' ' -f1 > /built/address",
		"{ curl -s -o /dev/null -w '%{http_code}' --max-time 30 http://" + standInName + "/debian/dists/bookworm/InRelease > /built/http; true; }",
	}, " && "))
	cfg.Packs = []string{m.Name}
	tag := workstationImage(t, []pack.Manifest{m})
	t.Cleanup(func() { docker("rmi", tag) })
	freshHome(t, cfg.ID)
	startUnbuilt(t, cfg, []pack.Manifest{m}, orchestrator.StartOptions{})
	waitState(t, cfg.ID, sandbox.StateGateOpen, 40*time.Second)

	read := func(file string) string {
		out, _ := inSandbox(cfg.ID, "cat /built/"+file)
		return trimmed(out)
	}
	if got := read("nonce"); got != nonce {
		t.Fatalf("the workstation's image was not built by this start: its step wrote %q, want %q", got, nonce)
	}
	found, err := netip.ParseAddr(read("address"))
	if err != nil || !sandbox.PublicAddress(found) {
		t.Errorf("the build's step was given %s at %q: by %s it is to be given the name's own address", standInName, read("address"), port)
	}
	if got := read("http"); got != "200" {
		t.Errorf("the build's step asked %s and was answered %q, want 200", standInName, got)
	}
}
