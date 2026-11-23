package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// A ruleset has the Hold's set and its rule only for a config that asks
// for a Hold. The rule passes what the Hold vouches for and nothing
// else: TCP that belongs to a connection that was answered, by the TUN
// device, to a public address, and never a SYN.
func TestRulesetWithAHold(t *testing.T) {
	without, err := ruleset(gateConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, "held") {
		t.Errorf("a ruleset with no Hold names one:\n%s", without)
	}

	cfg := gateConfig()
	cfg.Hold = true
	rules, err := ruleset(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rules, "set held { typeof meta mark; flags timeout; }") {
		t.Errorf("the ruleset lacks the Hold's set:\n%s", rules)
	}
	const rule = `oifname "tun0" ip daddr != @nonpublic4 tcp flags & (syn | ack) != syn ct state established meta mark @held counter name passed accept`
	chain := outputChain(t, rules)
	accepts, at, lease, reject := 0, -1, -1, -1
	for i, line := range chain {
		if strings.HasSuffix(line, "accept") {
			accepts++
		}
		switch {
		case line == rule:
			at = i
		case strings.Contains(line, "established") || strings.Contains(line, "@held"):
			t.Errorf("the output chain passes established flows, or looks up the Hold, in a rule that is not the Hold's: %s", line)
		case strings.Contains(line, "meta l4proto tcp meta mark @open"):
			lease = i
		case line == "meta l4proto tcp counter name rejected reject with tcp reset":
			reject = i
		}
	}
	if at < 0 {
		t.Fatalf("the output chain lacks the Hold's rule %q:\n%s", rule, strings.Join(chain, "\n"))
	}
	if accepts != 6 {
		t.Errorf("found %d accepts in the output chain, want 6: the five of every ruleset and the Hold's", accepts)
	}
	if lease < 0 || reject < 0 || at < lease || at > reject {
		t.Errorf("the Hold's rule is line %d, the lease's %d and the reject %d: it belongs between them", at, lease, reject)
	}
	// Whatever the config, the rules of a ruleset with no Hold are all
	// there, as they are.
	for _, line := range outputChain(t, without) {
		if !strings.Contains(strings.Join(chain, "\n"), line) {
			t.Errorf("the ruleset with a Hold lacks %q", line)
		}
	}
}

// answerDue is how long after an answer the next is due, at the tests'
// gates that keep a Hold, and holdLease how long such a gate's lease
// then runs from an answer that came in good time.
const (
	answerDue = 500 * time.Millisecond
	holdLease = 2*answerDue + sandbox.HoldLeaseMargin
)

// holdGate is a gate that keeps a Hold, with the app heard from.
func holdGate() (*gate, *fakeLease, *time.Time) {
	g, kernel, now := testGate()
	g.hold, g.answerDue = true, answerDue
	g.heartbeat(beat(true))
	return g, kernel, now
}

// at moves the clock to a moment, with the app heard from then.
func at(g *gate, now *time.Time, moment time.Time, h sandbox.Heartbeat) {
	*now = moment
	g.heartbeat(h)
}

// A gate that keeps no Hold renews none, whatever answers.
func TestGateWithoutAHoldKeepsNone(t *testing.T) {
	g, kernel, now := testGate()
	g.heartbeat(beat(true))
	g.answered(*now, 1, matched)
	if !kernel.held || kernel.holds != 0 || g.telemetry().Held {
		t.Fatalf("lease held %v, Hold renewed %d times, telemetry %+v", kernel.held, kernel.holds, g.telemetry())
	}
	sent := *now
	at(g, now, sent.Add(sandbox.LeaseDuration), beat(true))
	g.lapse()
	if tel := g.telemetry(); tel.Open || tel.Held || kernel.held {
		t.Fatalf("when the lease was over: lease held %v, telemetry %+v", kernel.held, tel)
	}
}

// The Hold is renewed with the lease, counted from the same probe. When
// nothing answers, the lease runs out and the gate is locked, and the
// Hold lasts: nothing is ended in the kernel, where each runs out by
// itself. When the Hold is over the gate says so, and ends both.
func TestGateKeepsAHoldWhenNothingAnswers(t *testing.T) {
	g, kernel, now := holdGate()
	told := 0
	g.changed = func() { told++ }
	var leaseDue, holdDue time.Duration
	g.renewed = func(d time.Duration) { leaseDue = d }
	g.held = func(d time.Duration) { holdDue = d }

	sent := *now
	*now = sent.Add(300 * time.Millisecond)
	g.answered(sent, 1, matched)
	if kernel.ttl != holdLease || kernel.holdTTL != sandbox.HoldDuration-300*time.Millisecond || leaseDue != kernel.ttl || holdDue != kernel.holdTTL {
		t.Fatalf("lease for %v, due in %v; Hold for %v, due in %v", kernel.ttl, leaseDue, kernel.holdTTL, holdDue)
	}
	// Open: the gate is not held, it is open.
	if tel := g.telemetry(); !tel.Open || tel.Held {
		t.Fatalf("under the lease the gate says %+v", tel)
	}

	// The lease is over, with no answer: locked, held, and nothing ended.
	// A gate that keeps a Hold has the shorter lease, counted from when
	// the answer came.
	ended, before := kernel.ended, told
	answered := sent.Add(300 * time.Millisecond)
	at(g, now, answered.Add(holdLease-time.Millisecond), beat(true))
	g.lapse()
	if tel := g.telemetry(); !tel.Open || tel.Held {
		t.Fatalf("a millisecond before its lease was over the gate says %+v", tel)
	}
	at(g, now, answered.Add(holdLease), beat(true))
	g.lapse()
	if tel := g.telemetry(); tel.Open || !tel.Held || tel.LockReason != sandbox.LockProbeUnanswered || kernel.ended != ended || !kernel.holding || told != before+1 {
		t.Fatalf("when the lease was over: telemetry %+v, ended %d times, Hold kept %v, told %d times", tel, kernel.ended-ended, kernel.holding, told-before)
	}
	// It lasts as long as it lasts, and says nothing new meanwhile.
	for _, after := range []time.Duration{6 * time.Second, 8 * time.Second, sandbox.HoldDuration - time.Millisecond} {
		at(g, now, sent.Add(after), beat(true))
		g.lapse()
		if tel := g.telemetry(); !tel.Held || kernel.ended != ended || told != before+1 {
			t.Fatalf("%v after the probe: telemetry %+v, ended %d times, told %d times", after, tel, kernel.ended-ended, told-before)
		}
	}

	// The Hold is over: said at once, and ended in the kernel.
	at(g, now, sent.Add(sandbox.HoldDuration), beat(true))
	g.lapse()
	if tel := g.telemetry(); tel.Open || tel.Held || tel.LockReason != sandbox.LockProbeUnanswered || kernel.ended != ended+1 || kernel.holding || told != before+2 {
		t.Fatalf("when the Hold was over: telemetry %+v, ended %d times, Hold kept %v, told %d times", tel, kernel.ended-ended, kernel.holding, told-before)
	}
}

// An answer that comes too late for the lease renews the Hold alone, for
// a Hold's duration from when its probe was sent. One older than a Hold
// lasts says nothing.
func TestALateAnswerRenewsTheHoldAlone(t *testing.T) {
	g, kernel, now := holdGate()
	sent := *now
	g.answered(sent, 1, matched)
	at(g, now, sent.Add(sandbox.LeaseDuration), beat(true))
	g.lapse()
	if !g.telemetry().Held {
		t.Fatal("the gate holds nothing once its lease is over")
	}

	// A probe sent three seconds in, answered after six more.
	renewed, holds := kernel.renewed, kernel.holds
	slow := sent.Add(3 * time.Second)
	at(g, now, sent.Add(9*time.Second), beat(true))
	g.answered(slow, 1, matched)
	if tel := g.telemetry(); tel.Open || !tel.Held || kernel.renewed != renewed || kernel.holds != holds+1 || kernel.holdTTL != sandbox.HoldDuration-6*time.Second {
		t.Fatalf("a late answer: telemetry %+v, lease renewed %d times, Hold %d times for %v", tel, kernel.renewed-renewed, kernel.holds-holds, kernel.holdTTL)
	}
	if tel := g.telemetry(); tel.ProbeLatency != matched.latency || tel.ExitAddress != matched.exit {
		t.Errorf("a late answer is shown as %+v", tel)
	}
	// An earlier probe's answer has nothing to add.
	g.answered(sent.Add(2*time.Second), 1, matched)
	if kernel.holds != holds+1 {
		t.Error("an answer to an earlier probe renewed the Hold")
	}
	// The Hold lasts past where the first would have ended.
	at(g, now, sent.Add(sandbox.HoldDuration+2*time.Second), beat(true))
	g.lapse()
	if !g.telemetry().Held {
		t.Error("the Hold a late answer renewed was over with the one before")
	}

	// Older than a Hold lasts: ignored.
	old := sent.Add(4 * time.Second)
	at(g, now, old.Add(sandbox.HoldDuration), beat(true))
	g.lapse()
	holds = kernel.holds
	g.answered(old, 1, matched)
	if tel := g.telemetry(); tel.Held || tel.Open || kernel.holds != holds {
		t.Fatalf("an answer older than a Hold lasts: telemetry %+v, Hold renewed %d times", tel, kernel.holds-holds)
	}

	// An answer in time opens the gate again, and renews both.
	fresh := *now
	*now = now.Add(time.Second)
	g.answered(fresh, 1, matched)
	if tel := g.telemetry(); !tel.Open || tel.Held || kernel.ttl != holdLease || kernel.holdTTL != sandbox.HoldDuration-time.Second {
		t.Fatalf("an answer in time: telemetry %+v, lease for %v, Hold for %v", tel, kernel.ttl, kernel.holdTTL)
	}
}

// Whatever locks the gate for a reason it knows ends the Hold with the
// lease, at once: only silence leaves it.
func TestWhatTheGateKnowsEndsTheHold(t *testing.T) {
	for _, c := range []struct {
		name   string
		lock   func(g *gate, now *time.Time)
		reason sandbox.LockReason
	}{
		{"a Host Condition that is unmet", func(g *gate, _ *time.Time) { g.heartbeat(beat(false)) }, sandbox.LockHostCondition},
		{"an answer with an address that is not expected", func(g *gate, now *time.Time) { g.answered(*now, 1, mismatched) }, sandbox.LockExitAddressMismatch},
		{"the end of the app's stream", func(g *gate, _ *time.Time) { g.heartbeatEnd() }, sandbox.LockHeartbeatLost},
		{"heartbeats that stopped", func(g *gate, now *time.Time) {
			*now = now.Add(sandbox.LeaseDuration + time.Second)
			g.lapse()
		}, sandbox.LockHeartbeatLost},
		{"no Interface in use", func(g *gate, _ *time.Time) {
			g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, InterfaceChange: 2})
		}, sandbox.LockNoInterface},
		{"another Interface in use", func(g *gate, _ *time.Time) {
			g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: "en1", InterfaceChange: 2})
		}, sandbox.LockProbeUnanswered},
	} {
		// Under an open gate, and under a Hold that lasts.
		for _, held := range []bool{false, true} {
			g, kernel, now := holdGate()
			sent := *now
			g.answered(sent, 1, matched)
			if held {
				at(g, now, sent.Add(sandbox.LeaseDuration+time.Second), beat(true))
				g.lapse()
				if !g.telemetry().Held {
					t.Fatalf("%s: the gate holds nothing once its lease is over", c.name)
				}
			}
			ended := kernel.ended
			c.lock(g, now)
			if tel := g.telemetry(); tel.Open || tel.Held || tel.LockReason != c.reason || kernel.holding || kernel.held || kernel.ended != ended+1 {
				t.Errorf("%s, held %v: telemetry %+v, lease held %v, Hold kept %v, ended %d times", c.name, held, tel, kernel.held, kernel.holding, kernel.ended-ended)
			}
		}
	}
}

// A gate that needs this Mac's physical exit addresses and does not know them
// holds nothing: not known is locked.
func TestPhysicalExitAddressesThatAreNotKnownEndTheHold(t *testing.T) {
	g, kernel, now := tunnelGate()
	g.hold = true
	g.heartbeat(tunnelBeat("203.0.113.7"))
	sent := *now
	g.answered(sent, 1, byTunnel)
	at(g, now, sent.Add(sandbox.LeaseDuration+time.Second), tunnelBeat("203.0.113.7"))
	g.lapse()
	if tel := g.telemetry(); !tel.Held || tel.LockReason != sandbox.LockProbeUnanswered {
		t.Fatalf("once the lease was over the gate says %+v", tel)
	}
	unknown := tunnelBeat()
	unknown.PhysicalExitKnown = false
	g.heartbeat(unknown)
	if tel := g.telemetry(); tel.Held || tel.LockReason != sandbox.LockPhysicalExitUnknown || kernel.holding {
		t.Fatalf("with this Mac's physical exit addresses unknown: telemetry %+v, Hold kept %v", tel, kernel.holding)
	}
	// And none is kept while they are not known.
	g.answered(*now, 1, byTunnel)
	if kernel.holding || kernel.held {
		t.Error("an answer renewed the lease or the Hold while they were unknown")
	}
}

// An answer with an address that is not expected ends the Hold however
// late it comes, when its probe was sent no earlier than the one the
// Hold is counted from. One to an earlier probe says nothing: a later
// probe was answered as expected. And no answer to a probe sent before
// it renews the Hold afterwards.
func TestALateAnswerThatIsNotExpectedEndsTheHold(t *testing.T) {
	g, kernel, now := holdGate()
	sent := *now
	g.answered(sent, 1, matched)
	at(g, now, sent.Add(sandbox.LeaseDuration), beat(true))
	g.lapse()

	// A probe sent before the one the Hold is counted from.
	earlier := sent.Add(-time.Second)
	at(g, now, sent.Add(6*time.Second), beat(true))
	g.answered(earlier, 1, mismatched)
	if tel := g.telemetry(); !tel.Held || !kernel.holding {
		t.Fatalf("a late answer to an earlier probe: telemetry %+v, Hold kept %v", tel, kernel.holding)
	}

	// A probe sent a second after it, answered six seconds later.
	later := sent.Add(time.Second)
	ended := kernel.ended
	at(g, now, sent.Add(7*time.Second), beat(true))
	g.answered(later, 1, mismatched)
	if tel := g.telemetry(); tel.Held || tel.Open || kernel.holding || kernel.ended != ended+1 {
		t.Fatalf("a late answer that is not expected: telemetry %+v, Hold kept %v, ended %d times", tel, kernel.holding, kernel.ended-ended)
	}

	// An answer as expected to a probe sent before that one renews
	// nothing; one to a probe sent after it does.
	holds := kernel.holds
	g.answered(sent.Add(500*time.Millisecond), 1, matched)
	if kernel.holds != holds || g.telemetry().Held {
		t.Fatal("an answer to a probe sent before the one that was not expected renewed the Hold")
	}
	g.answered(sent.Add(1500*time.Millisecond), 1, matched)
	if tel := g.telemetry(); kernel.holds != holds+1 || !tel.Held || tel.Open || kernel.holdTTL != sandbox.HoldDuration-5500*time.Millisecond {
		t.Fatalf("an answer to a later probe: Hold renewed %d times for %v, telemetry %+v", kernel.holds-holds, kernel.holdTTL, tel)
	}
}

// The address a Hold is counted from turns out to be this Mac's physical
// exit address, after its lease is over: the Hold is ended. With another
// answer since, whose address is still expected, it lasts.
func TestTheHoldEndsWhenItsAddressBecomesPhysical(t *testing.T) {
	g, kernel, now := tunnelGate()
	g.hold = true
	g.heartbeat(tunnelBeat())
	sent := *now
	g.answered(sent, 1, direct)
	at(g, now, sent.Add(6*time.Second), tunnelBeat())
	g.lapse()
	if !g.telemetry().Held {
		t.Fatal("the gate holds nothing once its lease is over")
	}
	g.heartbeat(tunnelBeat(direct.exit))
	if tel := g.telemetry(); tel.Held || tel.Open || kernel.holding {
		t.Fatalf("once the address was this Mac's physical exit address: telemetry %+v, Hold kept %v", tel, kernel.holding)
	}

	// The same, with a later answer by the tunnel's own address.
	g, kernel, now = tunnelGate()
	g.hold = true
	g.heartbeat(tunnelBeat())
	sent = *now
	g.answered(sent, 1, direct)
	*now = sent.Add(time.Second)
	g.answered(*now, 1, byTunnel)
	at(g, now, sent.Add(7*time.Second), tunnelBeat())
	g.lapse()
	g.heartbeat(tunnelBeat(direct.exit))
	if tel := g.telemetry(); !tel.Held || !kernel.holding {
		t.Fatalf("with a later answer that is still expected: telemetry %+v, Hold kept %v", tel, kernel.holding)
	}
}

// The kernel refuses to renew: locked, with nothing held.
func TestGateLocksWhenTheHoldIsRefused(t *testing.T) {
	g, kernel, now := holdGate()
	sent := *now
	g.answered(sent, 1, matched)
	at(g, now, sent.Add(sandbox.LeaseDuration), beat(true))
	g.lapse()
	kernel.fail = errors.New("nft: refused")
	at(g, now, sent.Add(8*time.Second), beat(true))
	g.answered(sent.Add(2*time.Second), 1, matched)
	if tel := g.telemetry(); tel.Held || tel.Open || kernel.holding || tel.LockReason != sandbox.LockProbeUnanswered {
		t.Fatalf("when the kernel refused: telemetry %+v, Hold kept %v", tel, kernel.holding)
	}
}

// At a gate that keeps a Hold the lease runs from an answer until two
// more were due and a margin more, and never longer than a lease's
// duration after the probe was sent: an answer that came fast keeps the
// gate open for that little, and one that took long for what is left of
// its probe's five seconds, so that a slow way out still opens the gate.
// Nothing else is shorter there: an answer with an address that is not
// expected is believed as long as at any gate, and a heartbeat may be as
// late.
func TestTheLeaseUnderAHoldIsCountedFromTheAnswer(t *testing.T) {
	if sandbox.HoldLeaseMargin <= 0 || holdLease >= sandbox.LeaseDuration {
		t.Fatalf("a gate that keeps a Hold adds %v to the time two answers are due in: its lease is shorter than one of %v", sandbox.HoldLeaseMargin, sandbox.LeaseDuration)
	}
	for _, c := range []struct {
		took, lease, plain time.Duration
	}{
		{200 * time.Millisecond, holdLease, sandbox.LeaseDuration - 200*time.Millisecond},
		{2 * time.Second, holdLease, 3 * time.Second},
		{sandbox.LeaseDuration - holdLease, holdLease, holdLease},
		{sandbox.LeaseDuration - time.Second, time.Second, time.Second},
	} {
		g, kernel, now := holdGate()
		sent := *now
		at(g, now, sent.Add(c.took), beat(true))
		g.answered(sent, 1, matched)
		if tel := g.telemetry(); !tel.Open || kernel.ttl != c.lease || tel.LeaseFor != c.lease {
			t.Errorf("an answer that took %v: telemetry %+v, lease for %v, want %v", c.took, tel, kernel.ttl, c.lease)
		}
		// The same answer at a gate that keeps none.
		plain, plainKernel, plainNow := testGate()
		plain.heartbeat(beat(true))
		sent = *plainNow
		at(plain, plainNow, sent.Add(c.took), beat(true))
		plain.answered(sent, 1, matched)
		if tel := plain.telemetry(); !tel.Open || plainKernel.ttl != c.plain {
			t.Errorf("at a gate that keeps no Hold, an answer that took %v: telemetry %+v, lease for %v, want %v", c.took, tel, plainKernel.ttl, c.plain)
		}
	}

	// A later answer renews the lease from when it came, also one to an
	// earlier probe: each says that the address was expected not five
	// seconds ago.
	g, kernel, now := holdGate()
	sent := *now
	*now = sent.Add(time.Second)
	g.answered(sent.Add(500*time.Millisecond), 1, matched)
	renewed := kernel.renewed
	*now = sent.Add(1200 * time.Millisecond)
	g.answered(sent, 1, matched)
	if kernel.renewed != renewed+1 || kernel.ttl != holdLease {
		t.Errorf("an answer to an earlier probe, 200ms later: lease renewed %d times, for %v", kernel.renewed-renewed, kernel.ttl)
	}

	// An answer that is not expected, older than the lease lasts from an
	// answer, locks the gate for the address, until a lease's duration
	// after its probe was sent.
	old := (holdLease + sandbox.LeaseDuration) / 2
	g, kernel, now = holdGate()
	sent = *now
	*now = sent.Add(time.Second)
	g.answered(sent, 1, matched)
	later := sent.Add(time.Second)
	at(g, now, later.Add(old), beat(true))
	g.answered(later, 1, mismatched)
	if tel := g.telemetry(); tel.Open || tel.Held || kernel.holding || tel.LockReason != sandbox.LockExitAddressMismatch {
		t.Fatalf("an answer that is not expected, %v old: telemetry %+v, Hold kept %v", old, tel, kernel.holding)
	}
	at(g, now, later.Add(sandbox.LeaseDuration-time.Millisecond), beat(true))
	g.answered(*now, 1, matched)
	if g.telemetry().Open {
		t.Error("an answer opened the gate while the address that was not expected was still believed")
	}
	at(g, now, later.Add(sandbox.LeaseDuration), beat(true))
	g.answered(*now, 1, matched)
	if !g.telemetry().Open {
		t.Error("no answer opened the gate once the address that was not expected was no longer believed")
	}

	// A heartbeat later than that lease lasts, and not later than a
	// lease lasts elsewhere, locks nothing.
	g, _, now = holdGate()
	*now = now.Add(old)
	g.answered(*now, 1, matched)
	g.lapse()
	if tel := g.telemetry(); !tel.Open {
		t.Fatalf("with a heartbeat %v old the gate says %+v", old, tel)
	}
	*now = now.Add(sandbox.LeaseDuration - old + time.Millisecond)
	g.lapse()
	if tel := g.telemetry(); tel.Open || tel.Held || tel.LockReason != sandbox.LockHeartbeatLost {
		t.Errorf("with a heartbeat older than a lease lasts the gate says %+v", tel)
	}
}
