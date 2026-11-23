//go:build integration

package integration

import (
	"testing"
	"time"

	"zeolite/internal/hostwatch"
	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// An exit drifts: the Interface stays up, with its name and its
// address, and what leaves by it begins to come out at another exit
// address. These tests need the virtual Interface's second exit address,
// and skip where the fixture has none.
//
// A drift either ends the connections that were made before it or
// leaves them where they were made, which is at the first exit address:
// a connection cannot move. Each test says which.

// theSecondExitAddress is the virtual Interface with its second exit
// address. The Interface is made to lead to the first again when the
// test ends.
func theSecondExitAddress(t *testing.T) (iface virtualInterface, second string) {
	t.Helper()
	iface = theVirtualInterface(t)
	second = iface.SecondExitAddress()
	if second == "" {
		t.Skip("the fixture has no second exit address that the virtual Interface can be made to lead to")
	}
	t.Cleanup(func() {
		iface.Keep(t, false)
		iface.Stall(t, false)
		iface.Drift(t, false)
	})
	return iface, second
}

// keeping names what a drift does with the connections made before it.
func keeping(kept bool) string {
	if kept {
		return "connections kept"
	}
	return "connections ended"
}

// sightingsAfter counts where a sandbox was seen, by a watch's answers:
// at the first exit address, at the second, and the last time it was
// seen at the second after a moment. An answer with any other
// address fails the test.
func sightingsAfter(t *testing.T, seen []sighting, iface virtualInterface, second string, since time.Time) (first, other int, last time.Duration) {
	t.Helper()
	for _, s := range seen {
		switch s.address {
		case iface.ExitAddress():
			first++
		case second:
			other++
			last = max(last, s.at.Sub(since))
		default:
			t.Errorf("the sandbox was seen at an address that is neither of the virtual Interface's exit addresses, %v after the change", s.at.Sub(since).Round(100*time.Millisecond))
		}
	}
	return first, other, last
}

// The Interface drifts to an address that is not expected, under a
// download: the sandbox lists the first exit address alone. The gate
// locks on the mismatch and names the other address, and the download
// gets no further: at once where the drift ended it, and at the lock
// where the drift left it. A Hold changes nothing of this: an address
// that is not expected ends it with the lease.
//
// Between the drift and the probe that sees it, the sandbox's new
// connections leave at the second exit address. The test counts the
// answers that came back from there, and how late the last was. A
// request that left there and was not answered before the lock, it does
// not see.
func TestVirtualInterfaceDriftToAnAddressThatIsNotExpected(t *testing.T) {
	iface, second := theSecondExitAddress(t)
	for _, hold := range []bool{false, true} {
		id, with := "virtual-drift", "without a Hold"
		if hold {
			id, with = "virtual-drift-hold", "with a Hold"
		}
		cfg := virtualInterfaceConfig(iface, id)
		cfg.Gate.Hold = hold
		start(t, cfg, nil, nil)
		noteLocks(t, id)
		capture := captureTUN(t, id)
		waitState(t, id, sandbox.StateGateOpen, 30*time.Second)

		for _, kept := range []bool{false, true} {
			what := with + ", " + keeping(kept)
			iface.Keep(t, kept)
			done, server, watch := underADownload(t, id, iface.ExitAddress())
			iface.Drift(t, true)
			drifted := time.Now()
			waitLockedFor(t, id, sandbox.LockExitAddressMismatch, 15*time.Second)
			locked := time.Since(drifted)
			if status := updates.statusOf(id); status.ExitAddress != second {
				t.Errorf("%s: the mismatch does not name the second exit address", what)
			}
			if gate := updates.gateOf(id); gate.Held || gate.Interface != iface.Name() {
				t.Errorf("%s: locked on the mismatch, the gate holds %v and the Interface in use is %q", what, gate.Held, gate.Interface)
			}
			// The download is over by the lock, Hold or none: a lease
			// later it would be the Hold that kept it.
			select {
			case ended := <-done:
				cut := ended.Sub(drifted)
				t.Logf("%s: the Interface drifted at %s; the gate locked %v after the drift, and the download ended %v after it", what, drifted.UTC().Format("15:04:05.000"), locked.Round(100*time.Millisecond), cut.Round(100*time.Millisecond))
				if !kept && cut > 2*time.Second {
					t.Errorf("%s: the download the drift ended went on for %v", what, cut.Round(100*time.Millisecond))
				}
			case <-time.After(3 * time.Second):
				t.Errorf("%s: the download went on for three seconds after the gate locked on the mismatch", what)
			}
			expectStopped(t, capture, server, "the drift of its Interface")
			if got := attempt(t, id, "1.1.1.1", 80); got == answered {
				t.Errorf("%s: a public address answered while the gate was locked on a mismatch", what)
			}

			first, other, last := sightingsAfter(t, watch.stop(t), iface, second, drifted)
			if first == 0 {
				t.Errorf("%s: the sandbox was never seen at the first exit address: the watch saw nothing", what)
			}
			t.Logf("%s: after the drift, the sandbox was answered %d times at the second exit address, the last %v after it", what, other, last.Round(100*time.Millisecond))
			if limit := sandbox.LeaseDuration + 2*time.Second; last > limit {
				t.Errorf("%s: the sandbox was still answered at the second exit address %v after the drift, want at most %v", what, last.Round(100*time.Millisecond), limit)
			}

			iface.Drift(t, false)
			back := time.Now()
			waitState(t, id, sandbox.StateGateOpen, 30*time.Second)
			t.Logf("%s: the gate was open again %v after the Interface led back", what, time.Since(back).Round(100*time.Millisecond))
			if got := sandboxSeenAt(id); got != iface.ExitAddress() {
				t.Errorf("%s: after the gate opened again the workstation is not seen at the first exit address", what)
			}
		}
		capture.expectGated(t)
		orch.Stop(t.Context(), id)
	}
}

// The Interface drifts to an address that is expected too: the sandbox
// lists both exit addresses, or expects any but physical. The gate
// stays open through the drift and through the way back. A download the
// drift left goes on, at the first exit address, and one the drift ended
// is the drift's doing and not the gate's: the next connection is made
// at once, at the second exit address.
func TestVirtualInterfaceDriftToAnAddressThatIsExpected(t *testing.T) {
	iface, second := theSecondExitAddress(t)
	for _, expects := range []string{"both exit addresses", "any but physical"} {
		id := "virtual-drift-both"
		cfg := newConfig(id, "Virtual Interface drift")
		cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: iface.Name(), ExpectedExitAddresses: []string{iface.ExitAddress(), second}}}
		var opts orchestrator.StartOptions
		if expects == "any but physical" {
			id = "virtual-drift-but-physical"
			cfg = newConfig(id, "Virtual Interface drift any but physical")
			cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: iface.Name(), AnyExitAddressButPhysical: true}}
			watchdog := hostwatch.New()
			watchdog.OnPhysicalExitChange(orch.BeatNow)
			gate := cfg.Gate
			opts.PhysicalExits = func(beside string) ([]string, bool) { return watchdog.PhysicalExitAddresses(beside, gate) }
		}
		startWith(t, cfg, nil, opts)
		noteLocks(t, id)
		capture := captureTUN(t, id)
		waitState(t, id, sandbox.StateGateOpen, 30*time.Second)
		// lockedSince fails the test for each time the gate was locked
		// since a moment.
		lockedSince := func(since time.Time, when string) {
			t.Helper()
			for _, s := range updates.since(id, since) {
				if s.State == sandbox.StateGateLocked {
					t.Errorf("expecting %s, %s: the gate locked for %q", expects, when, s.LockReason)
				}
			}
		}

		// The drift leaves the connections: nothing ends.
		iface.Keep(t, true)
		done, _, watch := underADownload(t, id, iface.ExitAddress())
		iface.Drift(t, true)
		drifted := time.Now()
		waitFor(t, 30*time.Second, "the watch to see the sandbox at the second exit address", func() bool {
			_, seen := watch.seen()
			return len(seen) > 0 && seen[len(seen)-1].address == second
		})
		t.Logf("expecting %s, connections kept: the sandbox was seen at the second exit address %v after the drift", expects, time.Since(drifted).Round(100*time.Millisecond))
		time.Sleep(2 * sandbox.LeaseDuration)
		lockedSince(drifted, "after a drift that kept the connections")
		select {
		case ended := <-done:
			t.Errorf("expecting %s: the download the drift left ended %v after the drift", expects, ended.Sub(drifted).Round(100*time.Millisecond))
		default:
		}
		iface.Drift(t, false)
		back := time.Now()
		waitFor(t, 30*time.Second, "the watch to see the sandbox at the first exit address again", func() bool {
			_, seen := watch.seen()
			return len(seen) > 0 && seen[len(seen)-1].address == iface.ExitAddress()
		})
		lockedSince(back, "after the way back")
		select {
		case <-done:
			t.Errorf("expecting %s: the download the drift left ended on the way back", expects)
		default:
		}

		// The drift ends the connections: the download is over, and the
		// gate is open all the same.
		iface.Keep(t, false)
		iface.Drift(t, true)
		drifted = time.Now()
		select {
		case ended := <-done:
			t.Logf("expecting %s, connections ended: the download ended %v after the drift, which ended it", expects, ended.Sub(drifted).Round(100*time.Millisecond))
		case <-time.After(10 * time.Second):
			t.Errorf("expecting %s: the download outlived a drift that ended the connections", expects)
		}
		if got := sandboxSeenAt(id); got != second {
			t.Errorf("expecting %s: after the drift the workstation is not seen at the second exit address", expects)
		}
		time.Sleep(sandbox.LeaseDuration)
		lockedSince(drifted, "after a drift that ended the connections")
		first, other, _ := sightingsAfter(t, watch.stop(t), iface, second, drifted)
		if first == 0 || other == 0 {
			t.Errorf("expecting %s: the watch saw the sandbox %d times at the first exit address and %d times at the second", expects, first, other)
		}
		iface.Drift(t, false)
		capture.expectGated(t)
		orch.Stop(t.Context(), id)
	}
}

// The virtual Interface stalls: for some seconds no new connection is
// made by its Interface, and the ones that were made go on. No probe is
// answered, and the lease runs out. Without a Hold the download is cut
// then, though its own connection never faltered. With one it goes on,
// and is still running when probes are answered again and the gate
// opens: what a Hold is for.
func TestVirtualInterfaceAHoldOutlastsAStall(t *testing.T) {
	iface, _ := theSecondExitAddress(t)
	for _, hold := range []bool{false, true} {
		id := "virtual-stall"
		if hold {
			id = "virtual-stall-hold"
		}
		cfg := virtualInterfaceConfig(iface, id)
		cfg.Gate.Hold = hold
		start(t, cfg, nil, nil)
		noteLocks(t, id)
		capture := captureTUN(t, id)
		waitState(t, id, sandbox.StateGateOpen, 30*time.Second)

		iface.Keep(t, true)
		done, server, watch := underADownload(t, id, iface.ExitAddress())
		iface.Stall(t, true)
		stalled := time.Now()
		if !hold {
			waitLockedFor(t, id, sandbox.LockProbeUnanswered, sandbox.LeaseDuration+5*time.Second)
			t.Logf("without a Hold: the gate locked %v after the stall", time.Since(stalled).Round(100*time.Millisecond))
			expectCut(t, done, stalled, sandbox.LeaseDuration+3*time.Second, "the stall, without a Hold")
			expectStopped(t, capture, server, "the lease ran out")
		} else {
			waitFor(t, sandbox.LeaseDuration+5*time.Second, "the gate to lock with its Hold lasting", func() bool {
				gate := updates.gateOf(id)
				return !gate.Open && gate.Held
			})
			locked := time.Since(stalled)
			// Probes are answered again while the Hold lasts.
			iface.Stall(t, false)
			waitState(t, id, sandbox.StateGateOpen, sandbox.HoldDuration)
			t.Logf("with a Hold: the gate locked %v after the stall, and was open again %v after it", locked.Round(100*time.Millisecond), time.Since(stalled).Round(100*time.Millisecond))
			time.Sleep(3 * time.Second)
			select {
			case ended := <-done:
				t.Errorf("with a Hold: the download ended %v after the stall", ended.Sub(stalled).Round(100*time.Millisecond))
			default:
			}
			before := capture.sentTo(t, server)
			time.Sleep(time.Second)
			if after := capture.sentTo(t, server); after <= before {
				t.Error("with a Hold: the download that outlasted the stall sends nothing")
			}
		}
		iface.Stall(t, false)
		waitState(t, id, sandbox.StateGateOpen, 30*time.Second)
		for _, s := range watch.stop(t) {
			if s.address != iface.ExitAddress() {
				t.Errorf("the sandbox was seen at an address that is not the first exit address, %v after the stall", s.at.Sub(stalled).Round(100*time.Millisecond))
			}
		}
		capture.expectGated(t)
		orch.Stop(t.Context(), id)
	}
}
