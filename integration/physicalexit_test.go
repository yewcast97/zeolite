//go:build integration

package integration

import (
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"zeolite/internal/hostwatch"
	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// An Allowed Interface may expect this Mac's physical exit address, or any
// exit address but a physical one. The app tells the gatekeeper which
// those are, in each heartbeat, and whether it knows them.

// physicalExits stands in for the Host Watchdog: this Mac's physical
// exit addresses are what a test says they are.
type physicalExits struct {
	mu    sync.Mutex
	addrs []string
	known bool
	// asked are the interfaces they were asked with, each the one that
	// was not to be counted among this Mac's physical Interfaces.
	asked map[string]int
}

func (o *physicalExits) ask(beside string) ([]string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.asked == nil {
		o.asked = map[string]int{}
	}
	o.asked[beside]++
	return append([]string(nil), o.addrs...), o.known
}

// say changes what this Mac's physical exit addresses are said to be, and
// has the gatekeepers told at once, as the app does when the Host
// Watchdog has read something new.
func (o *physicalExits) say(known bool, addrs ...string) time.Time {
	o.mu.Lock()
	o.addrs, o.known = addrs, known
	o.mu.Unlock()
	orch.BeatNow()
	return time.Now()
}

// A sandbox leaves by this Mac's physical Interface and expects any exit
// address but a physical one, which a stand-in says: so that each thing
// the gate does with them is seen against a real gatekeeper, a real
// Carrier and the real internet.
func TestGateExpectsAnyExitAddressButPhysical(t *testing.T) {
	exit := macExitAddress(t)
	physical := &physicalExits{}
	cfg := openConfig(t, "gate-but-physical")
	iface := cfg.AllowedInterfaces[0].Name
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: iface, AnyExitAddressButPhysical: true}}
	cfg.Gate.ProbeURLs = []string{publicProbeURL}
	startWith(t, cfg, nil, orchestrator.StartOptions{PhysicalExits: physical.ask})
	capture := captureTUN(t, cfg.ID)

	// Not known: locked for it, whatever the probe is answered, and
	// nothing leaves but the probe.
	waitLockedFor(t, cfg.ID, sandbox.LockPhysicalExitUnknown, 20*time.Second)
	if got := updates.statusOf(cfg.ID).String(); got != "GATE LOCKED [Physical Exit Address Unknown]" {
		t.Errorf("the status reads %q", got)
	}
	expectLockedOut(t, cfg.ID)
	time.Sleep(3 * time.Second) // rounds of probes, each answered with an address
	if status := updates.statusOf(cfg.ID); status.State != sandbox.StateGateLocked || status.LockReason != sandbox.LockPhysicalExitUnknown {
		t.Fatalf("after rounds of probes the sandbox is %+v, want it locked for %q", status, sandbox.LockPhysicalExitUnknown)
	}
	if tel := updates.gateOf(cfg.ID); tel.PhysicalExitKnown || len(tel.PhysicalExitAddresses) != 0 || tel.Interface != iface {
		t.Errorf("the gatekeeper says %+v", tel)
	}
	capture.expectProbesOnly(t)

	// Known, and another address than the one this sandbox leaves from:
	// the gate opens on whatever address the probe sees, which nobody
	// listed.
	physical.say(true, elsewhere)
	status := waitState(t, cfg.ID, sandbox.StateGateOpen, 15*time.Second)
	if status.ExitAddress != exit {
		t.Errorf("open on %q, want the address this Mac's network leaves from", status.ExitAddress)
	}
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code != 0 || trimmed(out) != exit {
		t.Fatalf("the workstation's address as the internet sees it: %q (exit %d), want %s", trimmed(out), code, exit)
	}
	waitFor(t, 5*time.Second, "the gatekeeper to report the physical exit addresses it took note of", func() bool {
		tel := updates.gateOf(cfg.ID)
		return tel.PhysicalExitKnown && strings.Join(tel.PhysicalExitAddresses, " ") == elsewhere
	})

	// The address the sandbox leaves from turns out to be this Mac's
	// physical exit address, under a download: the gate locks at once
	// on the mismatch and names the address, the download is cut, and
	// the Carrier holds no connection of the sandbox's.
	done, server := download(t, cfg.ID)
	held := "->" + server.String() + ":443"
	if now := carrierConnections(t); !strings.Contains(now, held) {
		t.Fatalf("the Carrier holds no connection to the download's server:\n%s", now)
	}
	said := physical.say(true, exit)
	waitLockedFor(t, cfg.ID, sandbox.LockExitAddressMismatch, 3*time.Second)
	t.Logf("the gate locked %v after the address was said to be this Mac's physical exit address", time.Since(said).Round(10*time.Millisecond))
	if status := updates.statusOf(cfg.ID); status.ExitAddress != exit {
		t.Errorf("the mismatch names %q, want the address this Mac's network leaves from", status.ExitAddress)
	}
	waitFor(t, 2*time.Second, "the Carrier to hold no connection to the download's server after the lock", func() bool {
		return !strings.Contains(carrierConnections(t), held)
	})
	expectCut(t, done, said, 6*time.Second, "the address becoming this Mac's physical exit address")
	expectStopped(t, capture, server, "the address became this Mac's physical exit address")
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code == 0 {
		t.Errorf("a request succeeded while the sandbox left from this Mac's physical exit address: %q", out)
	}
	// It stays locked for as long as the probe sees that address.
	time.Sleep(sandbox.LeaseDuration + time.Second)
	if status := updates.statusOf(cfg.ID); status.State != sandbox.StateGateLocked || status.LockReason != sandbox.LockExitAddressMismatch {
		t.Errorf("a lease later the sandbox is %+v, want it still locked on the mismatch", status)
	}

	// The address is not this Mac's physical exit address any more:
	// the gate opens once the mismatch is no longer believed.
	physical.say(true, elsewhere)
	waitState(t, cfg.ID, sandbox.StateGateOpen, sandbox.LeaseDuration+10*time.Second)

	// Not known any more, under a download: cut, by the heartbeat that
	// says so.
	done, server = download(t, cfg.ID)
	said = physical.say(false)
	waitLockedFor(t, cfg.ID, sandbox.LockPhysicalExitUnknown, 3*time.Second)
	t.Logf("the gate locked %v after this Mac's physical exit addresses were said to be unknown", time.Since(said).Round(10*time.Millisecond))
	expectCut(t, done, said, 6*time.Second, "this Mac's physical exit addresses becoming unknown")
	expectStopped(t, capture, server, "this Mac's physical exit addresses became unknown")
	if got := attempt(t, cfg.ID, "1.1.1.1", 80); got == answered {
		t.Error("a public address answered while this Mac's physical exit addresses were unknown")
	}

	// Known again: open again, and the workstation is seen where it was.
	physical.say(true)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 15*time.Second)
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code != 0 || trimmed(out) != exit {
		t.Errorf("after the gate opened again: %q (exit %d)", trimmed(out), code)
	}

	// They were asked for the Interface in use, and for no other.
	physical.mu.Lock()
	defer physical.mu.Unlock()
	if len(physical.asked) != 1 || physical.asked[iface] == 0 {
		t.Errorf("this Mac's physical exit addresses were asked for %v, want for %s alone", physical.asked, iface)
	}
	capture.expectGated(t)
}

// A sandbox leaves by this Mac's physical Interface and expects this Mac's
// physical exit address, which a stand-in says: the gate opens only while
// the probe sees one of those, against a real gatekeeper, a real
// Carrier and the real internet.
func TestGateExpectsThisMacsPhysicalExitAddress(t *testing.T) {
	exit := macExitAddress(t)
	physical := &physicalExits{}
	cfg := openConfig(t, "gate-physical")
	iface := cfg.AllowedInterfaces[0].Name
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: iface, PhysicalExitAddress: true}}
	cfg.Gate.ProbeURLs = []string{publicProbeURL}
	startWith(t, cfg, nil, orchestrator.StartOptions{PhysicalExits: physical.ask})
	capture := captureTUN(t, cfg.ID)

	// Not known: locked for it, whatever the probe is answered, and
	// nothing leaves but the probe.
	waitLockedFor(t, cfg.ID, sandbox.LockPhysicalExitUnknown, 20*time.Second)
	expectLockedOut(t, cfg.ID)
	time.Sleep(3 * time.Second)
	if status := updates.statusOf(cfg.ID); status.State != sandbox.StateGateLocked || status.LockReason != sandbox.LockPhysicalExitUnknown {
		t.Fatalf("after rounds of probes the sandbox is %+v, want it locked for %q", status, sandbox.LockPhysicalExitUnknown)
	}
	capture.expectProbesOnly(t)

	// Known, and another address than the one the sandbox leaves from,
	// as it would be at a tunnel's far end: locked on the mismatch,
	// which names the address the probe saw.
	physical.say(true, elsewhere)
	waitLockedFor(t, cfg.ID, sandbox.LockExitAddressMismatch, 10*time.Second)
	if status := updates.statusOf(cfg.ID); status.ExitAddress != exit {
		t.Errorf("the mismatch names %q, want the address the sandbox leaves from", status.ExitAddress)
	}
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code == 0 {
		t.Errorf("a request succeeded while the sandbox left from an address that is not this Mac's physical exit address: %q", out)
	}
	capture.expectProbesOnly(t)

	// The address the sandbox leaves from is this Mac's physical exit
	// address: the gate opens on it, once the mismatch is no longer
	// believed.
	physical.say(true, exit)
	status := waitState(t, cfg.ID, sandbox.StateGateOpen, sandbox.LeaseDuration+10*time.Second)
	if status.ExitAddress != exit {
		t.Errorf("open on %q, want this Mac's physical exit address", status.ExitAddress)
	}
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code != 0 || trimmed(out) != exit {
		t.Fatalf("the workstation's address as the internet sees it: %q (exit %d), want %s", trimmed(out), code, exit)
	}
	waitFor(t, 5*time.Second, "the gatekeeper to report the physical exit addresses it took note of", func() bool {
		tel := updates.gateOf(cfg.ID)
		return tel.PhysicalExitKnown && strings.Join(tel.PhysicalExitAddresses, " ") == exit
	})

	// The address stops being this Mac's physical exit address under a
	// download, as when what the Interface leads to changes: the gate
	// locks at once on the mismatch, and the download is cut.
	done, server := download(t, cfg.ID)
	said := physical.say(true, elsewhere)
	waitLockedFor(t, cfg.ID, sandbox.LockExitAddressMismatch, 3*time.Second)
	t.Logf("the gate locked %v after the address was said not to be this Mac's physical exit address", time.Since(said).Round(10*time.Millisecond))
	expectCut(t, done, said, 6*time.Second, "the address being this Mac's physical exit address no longer")
	expectStopped(t, capture, server, "the address was this Mac's physical exit address no longer")

	// Not known any more: locked for that.
	physical.say(false)
	waitLockedFor(t, cfg.ID, sandbox.LockPhysicalExitUnknown, 3*time.Second)
	// Known again, the sandbox's address among two: open again.
	physical.say(true, elsewhere, exit)
	waitState(t, cfg.ID, sandbox.StateGateOpen, sandbox.LeaseDuration+10*time.Second)

	// They were asked with no interface left out: every one of this
	// Mac's physical Interfaces counts, the Interface in use among
	// them.
	physical.mu.Lock()
	defer physical.mu.Unlock()
	if len(physical.asked) != 1 || physical.asked[""] == 0 {
		t.Errorf("this Mac's physical exit addresses were asked with %v left out, want with none", physical.asked)
	}
	capture.expectGated(t)
}

// With the real Host Watchdog, a sandbox that leaves by this Mac's
// physical Interface and expects this Mac's physical exit address opens:
// what its probe sees is what the Watchdog reads by the same interface.
func TestASandboxAtThisMacsPhysicalExitAddressOpens(t *testing.T) {
	exit := macExitAddress(t)
	cfg := openConfig(t, "gate-physical-watchdog")
	iface := cfg.AllowedInterfaces[0].Name
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: iface, PhysicalExitAddress: true}}
	watchdog := hostwatch.New()
	watchdog.OnPhysicalExitChange(orch.BeatNow)
	gate := cfg.Gate
	startWith(t, cfg, nil, orchestrator.StartOptions{
		PhysicalExits: func(beside string) ([]string, bool) { return watchdog.PhysicalExitAddresses(beside, gate) },
	})
	noteLocks(t, cfg.ID)
	capture := captureTUN(t, cfg.ID)
	status := waitState(t, cfg.ID, sandbox.StateGateOpen, 40*time.Second)
	if status.ExitAddress != exit {
		t.Errorf("open on %q, want the address this Mac's network leaves from", status.ExitAddress)
	}
	if tel := updates.gateOf(cfg.ID); !tel.PhysicalExitKnown || !slices.Contains(tel.PhysicalExitAddresses, exit) {
		t.Errorf("the gatekeeper took note of %d physical exit addresses (known %v), the sandbox's not among them", len(tel.PhysicalExitAddresses), tel.PhysicalExitKnown)
	}
	// It stays open over the Watchdog's next Rounds.
	time.Sleep(25 * time.Second)
	if status := updates.statusOf(cfg.ID); status.State != sandbox.StateGateOpen {
		t.Errorf("after the Watchdog's next Rounds the sandbox is %+v", status)
	}
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code != 0 || trimmed(out) != exit {
		t.Errorf("the workstation's address as the internet sees it: %q (exit %d), want %s", trimmed(out), code, exit)
	}
	capture.expectGated(t)
}

// A sandbox that lists its exit addresses is not asked about this Mac's
// physical exit address, and its gate opens with nobody to say them.
func TestAGateThatListsItsAddressesAsksForNoPhysicalExitAddress(t *testing.T) {
	physical := &physicalExits{}
	cfg := openConfig(t, "gate-listed")
	startWith(t, cfg, nil, orchestrator.StartOptions{PhysicalExits: physical.ask})
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	time.Sleep(2 * sandbox.HeartbeatInterval)
	physical.mu.Lock()
	defer physical.mu.Unlock()
	if len(physical.asked) != 0 {
		t.Errorf("this Mac's physical exit addresses were asked for %v by a sandbox that does not expect them", physical.asked)
	}
	if tel := updates.gateOf(cfg.ID); tel.PhysicalExitKnown || !tel.Open {
		t.Errorf("the gatekeeper says %+v", tel)
	}
}

// The real Host Watchdog reads this Mac's physical exit address: the one the
// test process is seen at, by the interface its requests take. An
// interface of this Mac that has no router, as the VM's host-only
// bridge, is not asked: it would never answer, and nothing would be
// known.
func TestTheWatchdogReadsThisMacsPhysicalExitAddress(t *testing.T) {
	exit := macExitAddress(t)
	iface, err := physicalInterface()
	if err != nil {
		t.Skipf("this Mac's physical Interface could not be found: %v", err)
	}
	var bridges []string
	if all, err := net.Interfaces(); err == nil {
		for _, ifi := range all {
			if strings.HasPrefix(ifi.Name, "bridge") && ifi.Flags&net.FlagUp != 0 {
				bridges = append(bridges, ifi.Name)
			}
		}
	}
	t.Logf("this Mac's physical Interface is %s; its bridges that are up: %v", iface, bridges)

	watchdog := hostwatch.New()
	told := make(chan struct{}, 16)
	watchdog.OnPhysicalExitChange(func() {
		select {
		case told <- struct{}{}:
		default:
		}
	})
	gate := sandbox.GateConfig{ProbeURLs: append([]string{publicProbeURL}, sandbox.DefaultProbeURLs...), Resolver: sandbox.DefaultResolver}

	// For a sandbox that leaves by another Interface: not known at once,
	// and known when the first probe is answered, which is told.
	began := time.Now()
	if addrs, known := watchdog.PhysicalExitAddresses("utun987", gate); known || len(addrs) != 0 {
		t.Fatalf("before anything was asked: %v, known %v", addrs, known)
	}
	select {
	case <-told:
	case <-time.After(15 * time.Second):
		t.Fatal("the Watchdog never told that it read a physical exit address")
	}
	addrs, known := watchdog.PhysicalExitAddresses("utun987", gate)
	t.Logf("this Mac's physical exit address was read in %v", time.Since(began).Round(10*time.Millisecond))
	if !known || len(addrs) != 1 || addrs[0] != exit {
		t.Fatalf("this Mac's physical exit addresses are read as %d addresses (known %v), want the one the test process is seen at", len(addrs), known)
	}

	// For a sandbox that leaves by this Mac's physical Interface there is no
	// other to read, where it is the only one.
	if addrs, known := watchdog.PhysicalExitAddresses(iface, gate); !known {
		t.Errorf("with %s in use: %d addresses, known %v", iface, len(addrs), known)
	}

	// Forgotten, as when the Mac wakes: not known, and read again.
	watchdog.Forget()
	if addrs, known := watchdog.PhysicalExitAddresses("utun987", gate); known || len(addrs) != 0 {
		t.Fatalf("after Forget: %v, known %v", addrs, known)
	}
	waitFor(t, 15*time.Second, "this Mac's physical exit address to be read again", func() bool {
		addrs, known := watchdog.PhysicalExitAddresses("utun987", gate)
		return known && len(addrs) == 1 && addrs[0] == exit
	})
}
