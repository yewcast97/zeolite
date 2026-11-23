package main

import (
	"maps"
	"slices"
	"sync"
	"time"

	"zeolite/internal/sandbox"
)

// lease is the timed kernel entry that keeps the gate open, and the
// Hold beside it, as the gate drives them.
type lease interface {
	// renew lets the sandbox's egress pass for lease from now, and the
	// connections it has for hold from now. One that is zero is left as
	// it is.
	renew(lease, hold time.Duration) error
	// end ends the lease, and the Hold with it: nothing passes.
	end() error
	// endFor ends them as end does, for an Interface in use that the
	// gate is about to take note of: in the same order it says whether
	// what passes under the lease that follows reaches the stand-in
	// addresses, which it does by an Interface where names are answered
	// with them, and by no other.
	endFor(standIns bool) error
}

// gate decides whether the sandbox's egress may pass, from the probes'
// answers and the app's heartbeats, and renews or ends the lease.
//
// An answer says where a connection exited when its probe was sent, and
// is believed for a lease's duration from then. One with an expected
// address renews the lease to that moment; one with another address
// ends it, and no answer renews it until that moment. A probe nobody
// answers says nothing: the lease runs out by itself.
//
// Where the Interface in use expects any exit address but a physical
// one, the app's heartbeats say which those are. While they do not say
// so the gate is locked, and an answer with one of them is an address
// that is not expected, from when its probe was sent. Where it expects
// this Mac's physical exit address, it is the other way round: an
// answer with any other address is the one that is not expected.
//
// The decision is enforced by the lease in the kernel alone: ending it
// stops every flow at its next packet, and it runs out by itself, so the
// gate locks without this process if it hangs.
//
// A gate that keeps a Hold believes an answer with an expected address
// for a Hold's duration besides, for the connections the sandbox has:
// those are kept while the Hold lasts, though the lease ran out, and
// what they send waits at the Carrier, which sends it on once the gate
// is open again. The Hold is renewed with the lease, and by an answer
// that came too late to renew the lease. Only silence leaves it: whatever locks the gate for
// a reason it knows ends the Hold with the lease, and so does an answer
// with an address that is not expected, however late, unless a later
// probe was answered as expected. The Hold too is a timed entry in the
// kernel, and runs out by itself.
type gate struct {
	lease lease
	now   func() time.Time
	// failed is told when the kernel refuses an order.
	failed func(error)
	// changed is told when what the gate says changed: whether it is
	// open, why it is locked, or which Interface in use it has taken note
	// of. The app hears of it at once, and not at the next report.
	changed func()
	// renewed is told how long the lease has to run from now, each time
	// it is renewed, and held how long the Hold has: lapse is due when
	// either is over. Both are called with the gate's lock held, and
	// must not call the gate.
	renewed func(time.Duration)
	held    func(time.Duration)
	// hold is whether the gate keeps a Hold, and answerDue how long
	// after an answer the next is due while the probes are answered in
	// time: the lease of a gate that keeps a Hold is counted from that.
	// They are set before the gate is used, as the two below.
	hold      bool
	answerDue time.Duration
	// butPhysical names the Interfaces that expect any exit address but a
	// physical one, and onlyPhysical those that expect this Mac's physical
	// exit address. They are set before the gate is used.
	butPhysical  map[string]bool
	onlyPhysical map[string]bool
	// standIns names the Interfaces where names are answered with
	// stand-in addresses. It is set before the gate is used.
	standIns map[string]bool

	mu             sync.Mutex
	heartbeatSeen  bool
	heartbeatEnded bool
	heartbeatAt    time.Time
	conditionsMet  bool
	// iface and change are the Interface in use and the count of its
	// changes that the gate has taken note of: the lease that was kept
	// before that change is ended. pending is true while a heartbeat has
	// named another pair and the kernel has not confirmed that end.
	iface   string
	change  uint64
	pending bool
	open    bool
	reason  sandbox.LockReason
	// ended is true while the kernel has confirmed the end of the lease
	// and nothing has renewed it since: there is nothing to end again.
	ended bool
	// told is what the gate said when changed was last told, or when it
	// was made.
	told said
	// until is when the lease runs out, while the gate is open.
	until time.Time
	// mismatchUntil is until when an answer with an address that is not
	// expected is believed: no answer renews the lease before then.
	mismatchUntil time.Time
	// heldFrom is when the probe was sent that the Hold is counted from,
	// and heldUntil when the Hold runs out. Both are zero while there is
	// none.
	heldFrom  time.Time
	heldUntil time.Time
	// mismatchAt is when the latest probe was sent that was answered
	// with an address that is not expected: no answer to a probe sent
	// before it renews the Hold.
	mismatchAt time.Time
	// exit and latency are what an answer said and how long it took, and
	// shown when the probe it answered was sent.
	exit    string
	latency time.Duration
	shown   time.Time
	// physical are this Mac's physical exit addresses as the last
	// heartbeat told them, and physicalKnown whether it said they are
	// known.
	physical      map[string]bool
	physicalKnown bool
	// answers are the addresses that were answered as expected while the
	// Interface in use expects this Mac's physical exit address, or any
	// but it, each with the latest probe so answered: when one of them
	// turns out to be this Mac's physical exit address, or not to be, it
	// was not expected from when that probe was sent.
	answers map[string]answerAt
}

// answerAt is when the probe of an answer was sent, and how long the
// answer took.
type answerAt struct {
	start   time.Time
	latency time.Duration
}

func newGate(l lease, failed func(error)) *gate {
	g := &gate{
		lease:   l,
		now:     time.Now,
		failed:  failed,
		changed: func() {},
		renewed: func(time.Duration) {},
		held:    func(time.Duration) {},
		reason:  sandbox.LockHeartbeatLost, // until the app is heard from
	}
	g.told = g.saidLocked()
	return g
}

// telemetry is the gate's state now, without the packet counters.
func (g *gate) telemetry() sandbox.GateTelemetry {
	g.mu.Lock()
	defer g.mu.Unlock()
	var leaseFor time.Duration
	if g.open {
		leaseFor = max(g.until.Sub(g.now()), 0)
	}
	return sandbox.GateTelemetry{
		Open:                  g.open,
		LeaseFor:              leaseFor,
		LockReason:            g.reason,
		Held:                  g.heldLocked(),
		Interface:             g.iface,
		InterfaceChange:       g.change,
		ExitAddress:           g.exit,
		ProbeLatency:          g.latency,
		PhysicalExitAddresses: slices.Sorted(maps.Keys(g.physical)),
		PhysicalExitKnown:     g.physicalKnown,
	}
}

// inUse is the Interface in use the gate has taken note of, and the
// count of its changes: what the next probe is a probe of.
func (g *gate) inUse() (iface string, change uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.iface, g.change
}

// said is what the gate says, as changed is told of it.
type said struct {
	open   bool
	held   bool
	reason sandbox.LockReason
	iface  string
	change uint64
}

func (g *gate) saidLocked() said { return said{g.open, g.heldLocked(), g.reason, g.iface, g.change} }

// heldLocked is whether the Hold lasts while the gate is locked.
func (g *gate) heldLocked() bool { return !g.open && g.now().Before(g.heldUntil) }

// belief is how long an answer says anything, from when its probe was
// sent: a lease's duration, or a Hold's where the gate keeps one.
func (g *gate) belief() time.Duration {
	if g.hold {
		return sandbox.HoldDuration
	}
	return sandbox.LeaseDuration
}

// leaseUntil is until when an answer with an expected address, which
// comes now, keeps the gate open: a lease's duration from when its
// probe was sent at start. A gate that keeps a Hold stays open no
// longer than until two more answers were due and a margin more: a
// lease that runs out there ends no connection, so it need outlast no
// more than one probe that is lost.
func (g *gate) leaseUntil(start time.Time) time.Time {
	until := start.Add(sandbox.LeaseDuration)
	if next := g.now().Add(2*g.answerDue + sandbox.HoldLeaseMargin); g.hold && next.Before(until) {
		until = next
	}
	return until
}

// tell releases the gate's lock, and tells changed if the gate says
// another thing than it last told. What it says changes with the time
// too, when a Hold is over: whoever takes the lock next tells of it.
func (g *gate) tell() {
	says := g.saidLocked()
	tell := says != g.told
	g.told = says
	g.mu.Unlock()
	if tell {
		g.changed()
	}
}

// heartbeat takes a heartbeat from the app. One that says the Host
// Conditions hold renews nothing: the next answer that matches does. One
// that says they do not locks the gate at once.
//
// A heartbeat that names another Interface in use than the gate has
// taken note of ends the lease first: nothing may cross the new
// Interface under the lease that was kept for the one before. The gate
// takes note of the new one only once the kernel has confirmed that the
// lease is ended, and says so at once. With that end the kernel is told
// whether the new one answers names with stand-in addresses: what
// passes under the next lease reaches those by such an Interface, and by
// no other.
//
// This Mac's physical exit addresses are taken from each heartbeat, once the
// Interface in use is settled. One that does not say they are known, or
// names what is no public IPv4 address, leaves them unknown, and
// whatever then forbids an open gate locks it at once.
func (g *gate) heartbeat(h sandbox.Heartbeat) {
	g.mu.Lock()
	g.heartbeatSeen = true
	g.heartbeatAt = g.now()
	g.conditionsMet = h.HostConditionsMet
	if h.Interface != g.iface || h.InterfaceChange != g.change {
		g.pending = true
		// The stand-in addresses are reached by the Interface that is
		// named now, or are not, from the moment the lease kept under
		// the one before is ended: the kernel takes both at once.
		if err := g.lease.endFor(g.standIns[h.Interface]); err != nil {
			// Not taken note of: the next heartbeat names it again. The
			// lease still runs out by itself within its duration.
			g.failed(err)
			g.open, g.reason, g.ended = false, sandbox.LockProbeUnanswered, false
			g.heldFrom, g.heldUntil = time.Time{}, time.Time{}
		} else {
			g.iface, g.change, g.pending = h.Interface, h.InterfaceChange, false
			// What the probes saw by the Interface before says nothing
			// of this one.
			g.open, g.reason, g.ended = false, sandbox.LockProbeUnanswered, true
			g.until, g.mismatchUntil = time.Time{}, time.Time{}
			g.heldFrom, g.heldUntil, g.mismatchAt = time.Time{}, time.Time{}, time.Time{}
			g.exit, g.latency, g.shown = "", 0, time.Time{}
			g.answers = nil
		}
	}
	if !g.pending {
		g.physicalLocked(h)
		if reason := g.standingLocked(); reason != "" {
			g.lockLocked(reason)
		}
	}
	g.tell()
}

// physicalLocked takes this Mac's physical exit addresses from a heartbeat,
// and judges again the answers that are still believed: one with an address
// that is now this Mac's physical exit address, where any but those is
// expected, or that is now known not to be, where those are, was not
// expected, from when its probe was sent. So no answer that is still on its
// way opens the gate again, and a Hold counted from such an answer is
// ended.
func (g *gate) physicalLocked(h sandbox.Heartbeat) {
	g.physical, g.physicalKnown = map[string]bool{}, h.PhysicalExitKnown
	for _, s := range h.PhysicalExitAddresses {
		a, err := sandbox.ParsePublicIPv4(s)
		if err != nil {
			// What cannot be read is not known.
			g.physical, g.physicalKnown = map[string]bool{}, false
			break
		}
		g.physical[a.String()] = true
	}
	for exit, at := range g.answers {
		if !g.now().Before(at.start.Add(g.belief())) {
			delete(g.answers, exit)
			continue
		}
		if !g.unexpectedLocked(exit) {
			continue
		}
		delete(g.answers, exit)
		if until := at.start.Add(sandbox.LeaseDuration); until.After(g.mismatchUntil) {
			g.mismatchUntil = until
			g.exit, g.latency, g.shown = exit, at.latency, at.start
		}
		g.contradictLocked(at.start)
	}
}

// contradictLocked takes note that the exit address was not expected
// when a probe was sent at start: no answer to a probe sent before then
// renews the Hold, and a Hold that is counted from a probe sent no later
// is ended. The lease that was renewed with such a Hold has run out, or
// is ended by what the caller does next: a Hold is counted from no
// earlier probe than its lease.
func (g *gate) contradictLocked(start time.Time) {
	if start.After(g.mismatchAt) {
		g.mismatchAt = start
	}
	if !g.now().Before(g.heldUntil) || start.Before(g.heldFrom) {
		return
	}
	reason := g.standingLocked()
	if reason == "" {
		// The lease is over, and nothing has answered since as expected.
		reason = sandbox.LockProbeUnanswered
	}
	g.lockLocked(reason)
}

// unexpectedLocked is whether an address the config lets pass is not
// expected at the Interface in use by what the app says of this Mac's
// physical exit addresses: one of them, where any but those is expected; or
// none of them, where those are, once they are known. While they are
// not known nothing is held against an address: the gate is locked for
// that, and says so.
func (g *gate) unexpectedLocked(exit string) bool {
	switch {
	case g.butPhysical[g.iface]:
		return g.physical[exit]
	case g.onlyPhysical[g.iface]:
		return g.physicalKnown && !g.physical[exit]
	}
	return false
}

// heartbeatEnd is called when the app's stream ends: the app is gone,
// and the gate stays locked.
func (g *gate) heartbeatEnd() {
	g.mu.Lock()
	g.heartbeatEnded = true
	g.lockLocked(sandbox.LockHeartbeatLost)
	g.tell()
}

// answered takes one answer to a probe that was sent at start, of the
// Interface in use whose count of changes was change then. A probe sent
// before the Interface in use changed says nothing of the one in use
// now, and an answer older than it is believed says nothing of now: both
// are ignored. An answer older than a lease lasts, which only a gate
// that keeps a Hold believes, says nothing to the lease: it renews the
// Hold, or ends it.
func (g *gate) answered(start time.Time, change uint64, a probeAnswer) {
	g.mu.Lock()
	defer g.tell()
	if change != g.change || g.pending || !g.now().Before(start.Add(g.belief())) {
		return
	}
	byPhysical := g.butPhysical[g.iface] || g.onlyPhysical[g.iface]
	if !a.matches || g.unexpectedLocked(a.exit) {
		// An address that is not expected is believed for a lease's
		// duration at any gate, whatever its own lease lasts.
		believed := start.Add(sandbox.LeaseDuration)
		if !g.now().Before(believed) {
			g.contradictLocked(start)
			return
		}
		if believed.After(g.mismatchUntil) {
			g.mismatchUntil = believed
		}
		g.contradictLocked(start)
		g.exit, g.latency, g.shown = a.exit, a.latency, start
		g.lockLocked(g.standingLocked())
		return
	}
	if byPhysical {
		// Kept for as long as it is believed, in case the app learns
		// that the address is this Mac's physical exit address, or
		// that it is not.
		if g.answers == nil {
			g.answers = map[string]answerAt{}
		}
		if at, kept := g.answers[a.exit]; !kept || start.After(at.start) {
			g.answers[a.exit] = answerAt{start, a.latency}
		}
	}
	if reason := g.standingLocked(); reason != "" {
		g.lockLocked(reason)
		return
	}
	until := g.leaseUntil(start)
	inTime := g.now().Before(until)
	// Of the answers to the probes sent together, the first is shown.
	if start.After(g.shown) {
		g.exit, g.latency, g.shown = a.exit, a.latency, start
	}
	// The lease is counted from when the probe was sent, and so is the
	// Hold. An answer to an earlier probe, or a second one to the same,
	// has nothing to add; one that came too late for the lease renews
	// the Hold alone, unless a later probe was answered with an address
	// that is not expected.
	var ttl, hold time.Duration
	if inTime && !(g.open && !until.After(g.until)) {
		ttl = until.Sub(g.now())
	}
	heldUntil := start.Add(sandbox.HoldDuration)
	if g.hold && start.After(g.mismatchAt) && heldUntil.After(g.heldUntil) {
		hold = heldUntil.Sub(g.now())
	}
	if ttl == 0 && hold == 0 {
		return
	}
	g.ended = false
	if err := g.lease.renew(ttl, hold); err != nil {
		g.failed(err)
		g.lockLocked(sandbox.LockProbeUnanswered)
		return
	}
	if hold > 0 {
		g.heldFrom, g.heldUntil = start, heldUntil
		g.held(hold)
	}
	if ttl > 0 {
		g.open, g.reason, g.until = true, "", until
		g.renewed(ttl)
	}
}

// lapse locks the gate when what kept it open is over: the lease ran
// out with no answer to renew it, or the heartbeats stopped. It is due
// when a lease runs out, when a Hold does, and before each round of
// probes: it also says why a gate that is locked is locked now, and ends
// once more a lease whose end the kernel refused.
//
// A lease that ran out with no answer leaves the Hold: the lease is
// over in the kernel by itself, and nothing is ended there until the
// Hold is over too.
func (g *gate) lapse() {
	g.mu.Lock()
	defer g.tell()
	reason := g.standingLocked()
	if reason == "" && g.open && g.now().Before(g.until) {
		return
	}
	if reason == "" && g.now().Before(g.heldUntil) {
		g.open, g.reason, g.until = false, sandbox.LockProbeUnanswered, time.Time{}
		return
	}
	if reason == "" {
		// Nothing forbids an open gate, and no answer has opened it.
		reason = sandbox.LockProbeUnanswered
	}
	g.lockLocked(reason)
}

// standingLocked is why the gate may not be open whatever a probe
// answers now, the weightiest reason first: the app is not heard from;
// there is no Interface in use; a Host Condition is unmet; the Interface
// in use expects this Mac's physical exit address, or any but it, and those
// are not known; a probe was answered with an address that is not
// expected. It is empty when nothing forbids an open gate.
func (g *gate) standingLocked() sandbox.LockReason {
	switch {
	case g.heartbeatEnded, !g.heartbeatSeen, g.now().Sub(g.heartbeatAt) > sandbox.LeaseDuration:
		return sandbox.LockHeartbeatLost
	case g.iface == "":
		return sandbox.LockNoInterface
	case !g.conditionsMet:
		return sandbox.LockHostCondition
	case (g.butPhysical[g.iface] || g.onlyPhysical[g.iface]) && !g.physicalKnown:
		return sandbox.LockPhysicalExitUnknown
	case g.now().Before(g.mismatchUntil):
		return sandbox.LockExitAddressMismatch
	}
	return ""
}

// lockLocked ends the lease and the Hold, unless the kernel has
// confirmed their end already, and says why.
func (g *gate) lockLocked(reason sandbox.LockReason) {
	if !g.ended {
		if err := g.lease.end(); err != nil {
			// Each still runs out by itself within its duration.
			g.failed(err)
		} else {
			g.ended = true
		}
	}
	g.open, g.reason, g.until = false, reason, time.Time{}
	g.heldFrom, g.heldUntil = time.Time{}, time.Time{}
}
