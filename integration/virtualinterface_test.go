//go:build integration

package integration

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"zeolite/internal/hostwatch"
	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// These tests need a virtual Interface of this Mac, beside its own, where
// a sandbox is seen at an exit address that is not this Mac's network's
// own: what it takes to tell one Interface from another, and to lose one
// under a sandbox that runs.
//
// What makes such an Interface is its owner's affair and not this
// repository's business: a fixture that is not part of it has one, and
// offers it here through newVirtualInterface. Where there is no fixture,
// these tests skip.

// virtualInterface is the virtual Interface as a test uses it. A fixture
// starts with it there, leading to its exit address, where the internet
// answers; a test that changes any of that changes it back.
type virtualInterface interface {
	// Name is the Interface's name.
	Name() string
	// ExitAddress is its exit address: where a sandbox is seen while it
	// leaves by the Interface.
	ExitAddress() string
	// Set says whether the Interface is there at all.
	Set(t *testing.T, there bool)
	// Divert says whether the Interface, still up, leads to this Mac's
	// physical Interface and no longer to its exit address.
	Divert(t *testing.T, diverted bool)
	// Stall says whether nothing is answered by the Interface any more,
	// while it is still up and still leads where it led.
	Stall(t *testing.T, stalled bool)
	// SecondExitAddress is a second exit address, where the fixture has
	// one that the Interface can be made to lead to, and empty where it
	// has none.
	SecondExitAddress() string
	// Drift says whether the Interface, still up, leads to the second
	// exit address and no longer to the first.
	Drift(t *testing.T, drifted bool)
	// Keep says what becomes, from now on, of the connections that were
	// made before the Interface is made to lead elsewhere: kept, they go
	// on where they were made; otherwise they are ended. It changes
	// nothing by itself.
	Keep(t *testing.T, kept bool)
}

// newVirtualInterface is set by a file outside the repository that has a
// virtual Interface to offer.
var newVirtualInterface func(t *testing.T) virtualInterface

func theVirtualInterface(t *testing.T) virtualInterface {
	t.Helper()
	if newVirtualInterface == nil {
		t.Skip("no virtual Interface is offered: its fixture is not part of the repository")
	}
	return newVirtualInterface(t)
}

// virtualInterfaceConfig is a sandbox that may leave by the second
// Interface, and is expected at its exit address there.
func virtualInterfaceConfig(iface virtualInterface, id string) sandbox.Config {
	cfg := newConfig(id, "Virtual Interface "+id)
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: iface.Name(), ExpectedExitAddresses: []string{iface.ExitAddress()}}}
	return cfg
}

// The gate opens on the virtual Interface's exit address, and the
// internet sees the workstation there.
func TestVirtualInterfaceExitAddress(t *testing.T) {
	iface := theVirtualInterface(t)
	cfg := virtualInterfaceConfig(iface, "virtual-open")
	cfg.Clock.Timezone = sandbox.TimezoneAuto
	cfg.Clock.TimezoneProbeURL = publicTimezoneURL
	port, connections := macListener(t)
	start(t, cfg, nil, nil)
	noteLocks(t, cfg.ID)
	capture := captureTUN(t, cfg.ID)

	status := waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	if status.ExitAddress != iface.ExitAddress() {
		t.Errorf("open on an exit address that is not the virtual Interface's")
	}
	waitFor(t, 10*time.Second, "telemetry with a probe latency", func() bool {
		return updates.gateOf(cfg.ID).ProbeLatency > 0
	})
	t.Logf("a probe by way of the virtual Interface takes %v", updates.gateOf(cfg.ID).ProbeLatency)

	// The workstation leaves from the virtual Interface's exit address,
	// to servers whose certificates it checks against the usual
	// authorities.
	out, code := inSandbox(cfg.ID, "curl -sS --max-time 15 "+publicProbeURL)
	if code != 0 || trimmed(out) != iface.ExitAddress() {
		t.Errorf("the workstation is not seen at the virtual Interface's exit address (exit %d)", code)
	}
	// Ports other than the web's pass too, and names resolve.
	if out, _ := inSandbox(cfg.ID, banner(publicSSHHost)); !strings.Contains(out, "SSH-") {
		t.Errorf("%s on port 22 said %q, want an SSH banner", publicSSHHost, trimmed(out))
	}

	// The timezone is that of the virtual Interface's exit address.
	tz, _ := inSandbox(cfg.ID, "echo $TZ")
	if _, err := time.LoadLocation(trimmed(tz)); err != nil || trimmed(tz) == "" || trimmed(tz) == sandbox.TimezoneAuto {
		t.Errorf("the workstation's timezone is %q, want the IANA timezone of the exit address", trimmed(tz))
	}
	t.Logf("the timezone of the exit address is %s", trimmed(tz))

	// The virtual Interface's exit address is itself no destination, and
	// neither is anything private.
	if out, _ := inSandbox(cfg.ID, banner(iface.ExitAddress())); strings.Contains(out, "SSH-") {
		t.Error("the workstation reached the SSH port at the virtual Interface's exit address")
	}
	if got := attempt(t, cfg.ID, hostGateway(t).String(), port); got == answered {
		t.Error("the Mac's listener answered a connection from the sandbox")
	}
	for _, addr := range vmAddresses(t) {
		if got := attempt(t, cfg.ID, addr.String(), 22); got == answered {
			t.Errorf("the VM at %s answered a connection from the sandbox", addr)
		}
	}
	if n := connections(); n != 0 {
		t.Errorf("the Mac's listener saw %d connections from the sandbox", n)
	}
	expectNoLeak(t, cfg.ID, "open")
	capture.expectGated(t)
}

// sighting is where the internet saw a sandbox at one moment.
type sighting struct {
	at      time.Time
	address string
}

// exitWatch asks the public probe URL, from a sandbox's workstation and
// again and again, what address it is seen at, each time on a connection
// of its own, and keeps the answers in a file there.
type exitWatch struct {
	sandboxID string
	ended     chan struct{}
}

// sightingsFile is where a workstation keeps what its watch was answered.
const sightingsFile = "/tmp/sightings"

func watchExit(t *testing.T, sandboxID string) *exitWatch {
	t.Helper()
	w := &exitWatch{sandboxID: sandboxID, ended: make(chan struct{})}
	go func() {
		defer close(w.ended)
		inSandbox(sandboxID, `rm -f /tmp/stop-watching; : > `+sightingsFile+`; while [ ! -e /tmp/stop-watching ]; do a=$(curl -s --max-time 5 `+publicProbeURL+`); echo "$(date +%s%3N) $a" >> `+sightingsFile+`; sleep 0.3; done`)
	}()
	return w
}

// seen returns how often the watch has asked so iface, and the answers
// that came.
func (w *exitWatch) seen() (asked int, seen []sighting) {
	text, _ := inSandbox(w.sandboxID, "cat "+sightingsFile+" 2>/dev/null")
	for _, line := range strings.Split(trimmed(text), "\n") {
		if line == "" {
			continue
		}
		asked++
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue // no answer came
		}
		var ms int64
		if _, err := fmt.Sscan(fields[0], &ms); err == nil {
			seen = append(seen, sighting{time.UnixMilli(ms), fields[1]})
		}
	}
	return asked, seen
}

// settle waits until the watch has seen the sandbox at an address and
// the sandbox's gate is open: what a test changes next, it changes
// under a sandbox that is leaving, and is seen where it should be. By
// way of the virtual Interface and beside a download an answer takes
// seconds, to the watch and to the probe alike, and not every one
// comes.
func (w *exitWatch) settle(t *testing.T, address string) {
	t.Helper()
	waitFor(t, 90*time.Second, "the watch to see the sandbox at the address it leaves from", func() bool {
		_, seen := w.seen()
		return len(seen) > 0 && seen[len(seen)-1].address == address
	})
	if status := updates.statusOf(w.sandboxID); status.State != sandbox.StateGateOpen {
		t.Logf("beside the download the gate was locked for %s", status.LockReason)
	}
	waitState(t, w.sandboxID, sandbox.StateGateOpen, 30*time.Second)
}

// underADownload starts a download in a sandbox and a watch of where it
// is seen, and returns once the sandbox is seen at an address with its
// gate open and the download still running. A gate that locks meanwhile,
// for want of any answer to its probes, ends the download: another is
// started then, so that what the test changes next, it changes under a
// download.
func underADownload(t *testing.T, sandboxID, address string) (done <-chan time.Time, server netip.Addr, watch *exitWatch) {
	t.Helper()
	watch = watchExit(t, sandboxID)
	for try := 1; ; try++ {
		done, server = download(t, sandboxID)
		watch.settle(t, address)
		select {
		case <-done:
			if try == 4 {
				t.Fatal("no download outlived the wait for the sandbox to be seen where it leaves from")
			}
			t.Log("a lock ended the download before anything was changed: another is started")
		default:
			return done, server, watch
		}
	}
}

// stop stops asking and returns the answers that came.
func (w *exitWatch) stop(t *testing.T) []sighting {
	t.Helper()
	inSandbox(w.sandboxID, "touch /tmp/stop-watching")
	select {
	case <-w.ended:
	case <-time.After(15 * time.Second):
		t.Error("the watch of the sandbox's exit address did not stop")
	}
	asked, seen := w.seen()
	t.Logf("the watch asked %d times and was answered %d times", asked, len(seen))
	return seen
}

// sandboxSeenAt asks, from a sandbox's workstation, where the internet
// sees it. By way of the virtual Interface not every answer comes: it
// asks until one does, three times at most. What curl says of a request
// that failed is no answer.
func sandboxSeenAt(sandboxID string) string {
	for range 3 {
		if out, _ := inSandbox(sandboxID, "curl -s --max-time 15 "+publicProbeURL); trimmed(out) != "" {
			return trimmed(out)
		}
	}
	return ""
}

// seenWhere says where an answer puts a sandbox, without the address.
func seenWhere(address string, iface virtualInterface, physicalExit string) string {
	switch address {
	case iface.ExitAddress():
		return "seen at the virtual Interface's exit address"
	case physicalExit:
		return "seen at this Mac's physical exit address"
	case "":
		return "not answered"
	}
	return "seen at an address that is neither the virtual Interface's nor this Mac's"
}

// virtualThenPhysical is a sandbox that may leave by the virtual Interface
// and then by this Mac's physical Interface, and is expected at each one's
// address.
func virtualThenPhysical(t *testing.T, iface virtualInterface, id string) (cfg sandbox.Config, physical, physicalExit string) {
	t.Helper()
	physicalExit = macExitAddress(t)
	physical, err := physicalInterface()
	if err != nil {
		t.Skipf("this Mac's physical Interface could not be found: %v", err)
	}
	cfg = virtualInterfaceConfig(iface, id)
	cfg.AllowedInterfaces = append(cfg.AllowedInterfaces, sandbox.AllowedInterface{Name: physical, ExpectedExitAddresses: []string{physicalExit}})
	return cfg, physical, physicalExit
}

// Two sandboxes leave by two Interfaces at once, and each is seen where
// its own leads. The loss of one's Interface is nothing to the other.
func TestVirtualInterfaceTwoSandboxesOnTwoInterfaces(t *testing.T) {
	iface := theVirtualInterface(t)
	physicalExit := macExitAddress(t)
	a, b := virtualInterfaceConfig(iface, "virtual-two-a"), openConfig(t, "virtual-two-b")
	for _, cfg := range []sandbox.Config{a, b} {
		start(t, cfg, nil, nil)
		noteLocks(t, cfg.ID)
	}
	for _, cfg := range []sandbox.Config{a, b} {
		waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	}
	if got := updates.gateOf(a.ID).Interface; got != iface.Name() {
		t.Errorf("the first sandbox's Interface in use is %q, want the virtual Interface", got)
	}
	if got := updates.gateOf(b.ID).Interface; got != b.AllowedInterfaces[0].Name {
		t.Errorf("the second sandbox's Interface in use is %q, want %q", got, b.AllowedInterfaces[0].Name)
	}
	// Both asked at the same moment.
	seen := make(chan string, 1)
	go func() { seen <- sandboxSeenAt(a.ID) }()
	if got := sandboxSeenAt(b.ID); got != physicalExit {
		t.Errorf("the sandbox on this Mac's physical Interface is not seen at this Mac's exit address: it is %s", seenWhere(got, iface, physicalExit))
	}
	if got := <-seen; got != iface.ExitAddress() {
		t.Errorf("the sandbox on the virtual Interface is not seen at the virtual Interface's exit address: it is %s", seenWhere(got, iface, physicalExit))
	}

	done, _ := download(t, b.ID)
	iface.Set(t, false)
	t.Cleanup(func() { iface.Set(t, true) })
	waitLockedFor(t, a.ID, sandbox.LockNoInterface, 10*time.Second)
	time.Sleep(3 * time.Second)
	select {
	case <-done:
		t.Error("a download by this Mac's physical Interface ended when another sandbox lost the virtual Interface")
	default:
	}
	if status := updates.statusOf(b.ID); status.State != sandbox.StateGateOpen {
		t.Errorf("the sandbox on this Mac's physical Interface is %+v after another lost its Interface", status)
	}
	if got := sandboxSeenAt(b.ID); got != physicalExit {
		t.Errorf("the sandbox on this Mac's physical Interface is no longer seen at this Mac's exit address: it is %s", seenWhere(got, iface, physicalExit))
	}

	iface.Set(t, true)
	waitState(t, a.ID, sandbox.StateGateOpen, 30*time.Second)
	if got := sandboxSeenAt(a.ID); got != iface.ExitAddress() {
		t.Errorf("after its Interface came back the first sandbox is not seen at the virtual Interface's exit address: it is %s", seenWhere(got, iface, physicalExit))
	}
}

// The one Interface a sandbox may leave by goes away under a download.
// The gate locks for No Interface and the download gets no further;
// nothing of the sandbox leaves this Mac, not even a probe, and at no
// moment is the sandbox seen at this Mac's own address: there is no
// falling back to an Interface that was not allowed. When the Interface
// is there again, the gate opens.
func TestVirtualInterfaceLostUnderADownload(t *testing.T) {
	iface := theVirtualInterface(t)
	physical := macExitAddress(t)
	cfg := virtualInterfaceConfig(iface, "virtual-lost")
	start(t, cfg, nil, nil)
	noteLocks(t, cfg.ID)
	capture := captureTUN(t, cfg.ID)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)

	_, server, watch := underADownload(t, cfg.ID, iface.ExitAddress())
	iface.Set(t, false)
	t.Cleanup(func() { iface.Set(t, true) })
	lost := time.Now()
	waitLockedFor(t, cfg.ID, sandbox.LockNoInterface, 10*time.Second)
	t.Logf("the gate was locked for No Interface %v after the Interface went", time.Since(lost).Round(100*time.Millisecond))
	if got := updates.gateOf(cfg.ID).Interface; got != "" {
		t.Errorf("with its one Interface gone the sandbox's Interface in use is %q", got)
	}
	expectStopped(t, capture, server, "the loss of its Interface")
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code == 0 {
		t.Errorf("a request succeeded with no Interface in use: %d bytes", len(out))
	}
	if got := attempt(t, cfg.ID, "1.1.1.1", 80); got == answered {
		t.Error("a public address answered with no Interface in use")
	}
	expectNoLeak(t, cfg.ID, "locked for want of an Interface")
	// Not a probe either: the device is silent.
	settled := len(capture.packets(t))
	time.Sleep(2 * sandbox.LeaseDuration)
	if after := capture.packets(t); len(after) != settled {
		t.Errorf("the sandbox sent %d packets with no Interface in use", len(after)-settled)
		for _, p := range after[settled:] {
			t.Logf("sent with no Interface in use: %s", p.line)
		}
	}

	seen := watch.stop(t)
	for _, s := range seen {
		if s.address == physical {
			t.Fatalf("the sandbox left from this Mac's physical exit address %v after its Interface went", s.at.Sub(lost).Round(100*time.Millisecond))
		}
		if s.address != iface.ExitAddress() {
			t.Error("the sandbox was seen at an address that is not the virtual Interface's")
		}
	}
	if len(seen) == 0 {
		t.Error("the sandbox was never seen at the virtual Interface's exit address: the watch saw nothing")
	}

	iface.Set(t, true)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	if got := sandboxSeenAt(cfg.ID); got != iface.ExitAddress() {
		t.Errorf("after the gate opened again the workstation is not seen at the virtual Interface's exit address: it is %s", seenWhere(got, iface, physical))
	}
	capture.expectGated(t)
}

// A sandbox may leave by the virtual Interface, then by this Mac's
// physical Interface. The first goes away under a download: the sandbox
// goes on by the second, where it is expected at this Mac's address,
// and the download does not move over. When the first is there again,
// the sandbox returns to it.
func TestVirtualInterfaceFallbackAndReturn(t *testing.T) {
	iface := theVirtualInterface(t)
	cfg, physical, physicalExit := virtualThenPhysical(t, iface, "virtual-fallback")
	start(t, cfg, nil, nil)
	noteLocks(t, cfg.ID)
	capture := captureTUN(t, cfg.ID)
	on := func(iface, exit string) func() bool {
		return func() bool {
			tel, status := updates.gateOf(cfg.ID), updates.statusOf(cfg.ID)
			return status.State == sandbox.StateGateOpen && tel.Open && tel.Interface == iface && status.ExitAddress == exit
		}
	}
	waitFor(t, 30*time.Second, "the gate to open on the virtual Interface", on(iface.Name(), iface.ExitAddress()))
	first := updates.gateOf(cfg.ID).InterfaceChange

	done, _, watch := underADownload(t, cfg.ID, iface.ExitAddress())
	iface.Set(t, false)
	t.Cleanup(func() { iface.Set(t, true) })
	lost := time.Now()
	waitFor(t, 30*time.Second, "the gate to open on this Mac's physical Interface", on(physical, physicalExit))
	t.Logf("the gate was open on the next Interface %v after the first went", time.Since(lost).Round(100*time.Millisecond))
	// A connection is of the Interface it was made on.
	expectCut(t, done, lost, sandbox.LeaseDuration+2*time.Second, "the loss of its Interface")
	if got := sandboxSeenAt(cfg.ID); got != physicalExit {
		t.Errorf("on this Mac's physical Interface the workstation is not seen at this Mac's exit address: it is %s", seenWhere(got, iface, physicalExit))
	}
	expectNoLeak(t, cfg.ID, "open on the virtual Interface")
	// The watch sees it there too, before the first Interface returns.
	watch.settle(t, physicalExit)

	iface.Set(t, true)
	back := time.Now()
	waitFor(t, 30*time.Second, "the gate to open on the virtual Interface again", on(iface.Name(), iface.ExitAddress()))
	t.Logf("the gate was open on the first Interface again %v after it came back", time.Since(back).Round(100*time.Millisecond))
	if got := sandboxSeenAt(cfg.ID); got != iface.ExitAddress() {
		t.Errorf("back on the virtual Interface the workstation is not seen at the virtual Interface's exit address: it is %s", seenWhere(got, iface, physicalExit))
	}
	if change := updates.gateOf(cfg.ID).InterfaceChange; change < first+2 {
		t.Errorf("the Interface in use changed %d times, want at least 2", change-first)
	}

	// What the internet saw meanwhile: the virtual Interface's exit
	// address, then this Mac's, then the virtual Interface's, and no
	// other.
	var places []string
	for _, s := range watch.stop(t) {
		place := "elsewhere"
		switch s.address {
		case iface.ExitAddress():
			place = "the virtual Interface"
		case physicalExit:
			place = "this Mac"
		}
		if len(places) == 0 || places[len(places)-1] != place {
			places = append(places, place)
		}
	}
	if got := strings.Join(places, ", then "); got != "the virtual Interface, then this Mac, then the virtual Interface" && got != "the virtual Interface, then this Mac" {
		t.Errorf("the sandbox was seen at %s", got)
	}
	capture.expectGated(t)
}

// The virtual Interface stays up and begins to lead to this Mac's
// physical Interface, under a download. Nothing says so but the
// probe: the gate locks on the mismatch and the download gets no
// further.
//
// Between the diversion and the probe that sees it, a new connection of
// the sandbox's leaves from this Mac's own address: where an Interface
// leads is the Interface's business. This is the window that remains,
// and the test measures how long it was.
func TestVirtualInterfaceDivertedUnderADownload(t *testing.T) {
	iface := theVirtualInterface(t)
	physical := macExitAddress(t)
	cfg := virtualInterfaceConfig(iface, "virtual-diverted")
	start(t, cfg, nil, nil)
	noteLocks(t, cfg.ID)
	capture := captureTUN(t, cfg.ID)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)

	_, server, watch := underADownload(t, cfg.ID, iface.ExitAddress())
	iface.Divert(t, true)
	t.Cleanup(func() { iface.Divert(t, false) })
	diverted := time.Now()
	waitLockedFor(t, cfg.ID, sandbox.LockExitAddressMismatch, 15*time.Second)
	if status := updates.statusOf(cfg.ID); status.ExitAddress == iface.ExitAddress() || status.ExitAddress == "" {
		t.Error("the mismatch does not name another address than the virtual Interface's")
	}
	if got := updates.gateOf(cfg.ID).Interface; got != iface.Name() {
		t.Errorf("the Interface in use is %q, want the virtual Interface, which is still up", got)
	}
	expectStopped(t, capture, server, "the diversion of its Interface")
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code == 0 {
		t.Errorf("a request succeeded through a gate locked on a mismatch: %d bytes", len(out))
	}
	if got := attempt(t, cfg.ID, "1.1.1.1", 80); got == answered {
		t.Error("a public address answered while the gate was locked on a mismatch")
	}
	expectNoLeak(t, cfg.ID, "locked on a mismatch")

	// What the internet saw of the sandbox meanwhile: the virtual
	// Interface's address before the diversion, and after it this Mac's
	// physical exit address for as long as the gate took to see the
	// mismatch, which is at most a round of probes and an answer.
	before, elsewhere, last := 0, 0, time.Duration(0)
	for _, s := range watch.stop(t) {
		switch {
		case s.address == iface.ExitAddress():
			before++
		case s.address == physical:
			elsewhere++
			last = max(last, s.at.Sub(diverted))
		default:
			t.Error("the sandbox was seen at an address that is neither the virtual Interface's nor this Mac's physical exit address")
		}
	}
	if before == 0 {
		t.Error("the sandbox was never seen at the virtual Interface's exit address: the watch saw nothing")
	}
	t.Logf("after the diversion, %d connections of the sandbox left from this Mac's physical exit address, the last %v after it", elsewhere, last.Round(100*time.Millisecond))
	if limit := sandbox.LeaseDuration + 2*time.Second; last > limit {
		t.Errorf("the sandbox still left from this Mac's physical exit address %v after the diversion, want at most %v", last.Round(100*time.Millisecond), limit)
	}

	iface.Divert(t, false)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	if got := sandboxSeenAt(cfg.ID); got != iface.ExitAddress() {
		t.Errorf("after the gate opened again the workstation is not seen at the virtual Interface's exit address: it is %s", seenWhere(got, iface, physical))
	}
	capture.expectGated(t)
}

// The sandbox may leave by the virtual Interface alone, and expects there
// any exit address but a physical one: nobody lists the second Interface's
// address. The Host Watchdog is the real one, wired as the app wires it: it
// reads this Mac's physical exit address by this Mac's physical Interface,
// beside the sandbox's Interface.
//
// The gate opens on the virtual Interface's exit address. When the Mac
// wakes, what the Watchdog read is forgotten and the gate is locked
// until it has read again. And when the Interface, still up, begins to
// lead to this Mac's physical Interface, the gate locks on the mismatch and
// names this Mac's address, which nobody listed either.
func TestVirtualInterfaceAnyExitAddressButPhysical(t *testing.T) {
	iface := theVirtualInterface(t)
	physical := macExitAddress(t)
	cfg := newConfig("virtual-but-physical", "Virtual Interface any but physical")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: iface.Name(), AnyExitAddressButPhysical: true}}
	watchdog := hostwatch.New()
	watchdog.OnPhysicalExitChange(orch.BeatNow)
	gate := cfg.Gate
	startWith(t, cfg, nil, orchestrator.StartOptions{
		PhysicalExits: func(beside string) ([]string, bool) { return watchdog.PhysicalExitAddresses(beside, gate) },
	})
	noteLocks(t, cfg.ID)
	capture := captureTUN(t, cfg.ID)

	status := waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	if status.ExitAddress != iface.ExitAddress() {
		t.Errorf("open on an exit address that is not the virtual Interface's: it is %s", seenWhere(status.ExitAddress, iface, physical))
	}
	// The gatekeeper was told this Mac's physical exit address, and no other.
	waitFor(t, 10*time.Second, "the gatekeeper to report this Mac's physical exit address", func() bool {
		tel := updates.gateOf(cfg.ID)
		return tel.PhysicalExitKnown && len(tel.PhysicalExitAddresses) == 1 && tel.PhysicalExitAddresses[0] == physical
	})
	if got := sandboxSeenAt(cfg.ID); got != iface.ExitAddress() {
		t.Errorf("the workstation is %s, want it seen at the virtual Interface's exit address", seenWhere(got, iface, physical))
	}

	// The Mac sleeps and wakes, as the app is told it: asleep the gate is
	// locked for the Host Condition; awake the Watchdog has forgotten
	// what it read, the gate is locked for that, and opens once this
	// Mac's physical exit address is read again.
	orch.SetSuspended(true)
	waitLockedFor(t, cfg.ID, sandbox.LockHostCondition, 4*time.Second)
	woke := time.Now()
	watchdog.Forget()
	orch.SetSuspended(false)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	t.Logf("the gate was open again %v after the Mac woke", time.Since(woke).Round(100*time.Millisecond))
	unknown := false
	for _, s := range updates.since(cfg.ID, woke) {
		unknown = unknown || s.State == sandbox.StateGateLocked && s.LockReason == sandbox.LockPhysicalExitUnknown
		if s.State == sandbox.StateGateOpen && !unknown {
			t.Error("after the Mac woke the gate opened without having been locked for Physical Exit Address Unknown")
		}
	}
	if !unknown {
		t.Errorf("after the Mac woke the gate was never locked for %q: it was %+v", sandbox.LockPhysicalExitUnknown, updates.since(cfg.ID, woke))
	}

	// The Interface is diverted under a download.
	_, server, watch := underADownload(t, cfg.ID, iface.ExitAddress())
	iface.Divert(t, true)
	t.Cleanup(func() { iface.Divert(t, false) })
	diverted := time.Now()
	waitLockedFor(t, cfg.ID, sandbox.LockExitAddressMismatch, 15*time.Second)
	t.Logf("the gate locked %v after the diversion", time.Since(diverted).Round(100*time.Millisecond))
	if status := updates.statusOf(cfg.ID); status.ExitAddress != physical {
		t.Errorf("the mismatch names an address that is %s, want this Mac's physical exit address", seenWhere(status.ExitAddress, iface, physical))
	}
	if got := updates.gateOf(cfg.ID).Interface; got != iface.Name() {
		t.Errorf("the Interface in use is %q, want the virtual Interface, which is still up", got)
	}
	expectStopped(t, capture, server, "the diversion of its Interface")
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code == 0 {
		t.Errorf("a request succeeded through a gate locked on a mismatch: %d bytes", len(out))
	}
	expectNoLeak(t, cfg.ID, "locked on a mismatch")

	// For how long the internet saw the sandbox at this Mac's physical
	// exit address: the window that remains, which is at most a round
	// of probes and an answer.
	before, elsewhere, last := 0, 0, time.Duration(0)
	for _, s := range watch.stop(t) {
		switch {
		case s.address == iface.ExitAddress():
			before++
		case s.address == physical:
			elsewhere++
			last = max(last, s.at.Sub(diverted))
		default:
			t.Error("the sandbox was seen at an address that is neither the virtual Interface's nor this Mac's physical exit address")
		}
	}
	if before == 0 {
		t.Error("the sandbox was never seen at the virtual Interface's exit address: the watch saw nothing")
	}
	t.Logf("after the diversion, %d connections of the sandbox left from this Mac's physical exit address, the last %v after it", elsewhere, last.Round(100*time.Millisecond))
	if limit := sandbox.LeaseDuration + 2*time.Second; last > limit {
		t.Errorf("the sandbox still left from this Mac's physical exit address %v after the diversion, want at most %v", last.Round(100*time.Millisecond), limit)
	}

	// It stays locked for as long as the Interface leads there, and
	// opens when it leads to its exit address again.
	time.Sleep(sandbox.LeaseDuration + time.Second)
	if status := updates.statusOf(cfg.ID); status.State != sandbox.StateGateLocked || status.LockReason != sandbox.LockExitAddressMismatch {
		t.Errorf("a lease after the diversion the sandbox is %s %s, want it still locked on the mismatch", status.State, status.LockReason)
	}
	iface.Divert(t, false)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	if got := sandboxSeenAt(cfg.ID); got != iface.ExitAddress() {
		t.Errorf("after the gate opened again the workstation is %s, want it seen at the virtual Interface's exit address", seenWhere(got, iface, physical))
	}
	capture.expectGated(t)
}

// By the virtual Interface a sandbox that expects this Mac's physical exit
// address stays locked: it is seen at the virtual Interface's exit address,
// which is not this Mac's physical exit address, and nothing of it leaves
// but its probes. When the Interface is diverted, so that it leads where
// this Mac's physical Interfaces do, the gate opens, at this Mac's physical
// exit address; and it locks again when the Interface leads to its exit
// address again.
func TestVirtualInterfaceThisMacsPhysicalExitAddress(t *testing.T) {
	iface := theVirtualInterface(t)
	physical := macExitAddress(t)
	cfg := newConfig("virtual-physical", "Virtual Interface physical")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: iface.Name(), PhysicalExitAddress: true}}
	watchdog := hostwatch.New()
	watchdog.OnPhysicalExitChange(orch.BeatNow)
	gate := cfg.Gate
	startWith(t, cfg, nil, orchestrator.StartOptions{
		PhysicalExits: func(beside string) ([]string, bool) { return watchdog.PhysicalExitAddresses(beside, gate) },
	})
	capture := captureTUN(t, cfg.ID)

	// Locked on the mismatch, once this Mac's physical exit address is read:
	// the address the probe saw is the virtual Interface's.
	waitLockedFor(t, cfg.ID, sandbox.LockExitAddressMismatch, 40*time.Second)
	if status := updates.statusOf(cfg.ID); status.ExitAddress != iface.ExitAddress() {
		t.Errorf("the mismatch names an address that is %s, want the virtual Interface's", seenWhere(status.ExitAddress, iface, physical))
	}
	waitFor(t, 10*time.Second, "the gatekeeper to report this Mac's physical exit address", func() bool {
		tel := updates.gateOf(cfg.ID)
		return tel.PhysicalExitKnown && len(tel.PhysicalExitAddresses) == 1 && tel.PhysicalExitAddresses[0] == physical
	})
	// It stays locked, and nothing leaves but the probes.
	time.Sleep(sandbox.LeaseDuration + 2*time.Second)
	for _, s := range updates.since(cfg.ID, time.Time{}) {
		if s.State == sandbox.StateGateOpen {
			t.Error("the gate was open by the virtual Interface, which does not lead to this Mac's physical exit address")
		}
	}
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code == 0 {
		t.Errorf("a request succeeded through a gate locked on a mismatch: %d bytes", len(out))
	}
	expectNoLeak(t, cfg.ID, "locked on a mismatch")
	capture.expectProbesOnly(t)

	// Diverted: the Interface leads where this Mac's physical Interfaces
	// do.
	iface.Divert(t, true)
	t.Cleanup(func() { iface.Divert(t, false) })
	status := waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	if status.ExitAddress != physical {
		t.Errorf("open on an address that is %s, want this Mac's physical exit address", seenWhere(status.ExitAddress, iface, physical))
	}
	if got := sandboxSeenAt(cfg.ID); got != physical {
		t.Errorf("the workstation is %s, want it seen at this Mac's physical exit address", seenWhere(got, iface, physical))
	}

	// It leads to its exit address again, under a download: locked
	// again, and the download gets no further.
	_, server, watch := underADownload(t, cfg.ID, physical)
	iface.Divert(t, false)
	back := time.Now()
	waitLockedFor(t, cfg.ID, sandbox.LockExitAddressMismatch, 15*time.Second)
	t.Logf("the gate locked %v after the Interface led to its exit address again", time.Since(back).Round(100*time.Millisecond))
	expectStopped(t, capture, server, "its Interface leading to its exit address again")
	elsewhere, last := 0, time.Duration(0)
	for _, s := range watch.stop(t) {
		if s.address == iface.ExitAddress() {
			elsewhere++
			last = max(last, s.at.Sub(back))
		}
	}
	t.Logf("after that, %d connections of the sandbox left from the virtual Interface's exit address, the last %v after it", elsewhere, last.Round(100*time.Millisecond))
	if limit := sandbox.LeaseDuration + 2*time.Second; last > limit {
		t.Errorf("the sandbox still left from the virtual Interface's exit address %v after the Interface led there, want at most %v", last.Round(100*time.Millisecond), limit)
	}
	capture.expectGated(t)
}

// The virtual Interface stops answering under a download, and the
// Interface is still up: the sandbox does not go on to the next
// Interface it is allowed, which is for an Interface that is gone. The
// probe goes unanswered, the gate locks, the download gets no further,
// and at no moment is the sandbox seen anywhere but at the second
// Interface's exit address.
func TestVirtualInterfaceSilentUnderADownload(t *testing.T) {
	iface := theVirtualInterface(t)
	cfg, _, physicalExit := virtualThenPhysical(t, iface, "virtual-silent")
	start(t, cfg, nil, nil)
	noteLocks(t, cfg.ID)
	capture := captureTUN(t, cfg.ID)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)

	_, server, watch := underADownload(t, cfg.ID, iface.ExitAddress())
	iface.Stall(t, true)
	t.Cleanup(func() { iface.Stall(t, false) })
	silent := time.Now()
	waitLockedFor(t, cfg.ID, sandbox.LockProbeUnanswered, 15*time.Second)
	expectStopped(t, capture, server, "the virtual Interface's silence")
	if got := attempt(t, cfg.ID, "1.1.1.1", 80); got == answered {
		t.Error("a public address answered while the gate was locked for want of a probe")
	}
	expectNoLeak(t, cfg.ID, "locked for want of a probe")
	// A round of probes later the Interface in use is the same.
	time.Sleep(sandbox.LeaseDuration)
	if status := updates.statusOf(cfg.ID); status.State != sandbox.StateGateLocked || status.LockReason != sandbox.LockProbeUnanswered {
		t.Errorf("with the virtual Interface silent the sandbox is %+v", status)
	}
	if got := updates.gateOf(cfg.ID).Interface; got != iface.Name() {
		t.Errorf("with the virtual Interface silent the Interface in use is %q, want the virtual Interface still", got)
	}

	seen := watch.stop(t)
	for _, s := range seen {
		if s.address == physicalExit {
			t.Fatalf("the sandbox left from this Mac's physical exit address %v after the virtual Interface went silent", s.at.Sub(silent).Round(100*time.Millisecond))
		}
		if s.address != iface.ExitAddress() {
			t.Error("the sandbox was seen at an address that is not the virtual Interface's")
		}
	}
	if len(seen) == 0 {
		t.Error("the sandbox was never seen at the virtual Interface's exit address: the watch saw nothing")
	}

	iface.Stall(t, false)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	if got := sandboxSeenAt(cfg.ID); got != iface.ExitAddress() {
		t.Errorf("after the gate opened again the workstation is not seen at the virtual Interface's exit address: it is %s", seenWhere(got, iface, physicalExit))
	}
	capture.expectGated(t)
}
