package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/netip"
	"strings"
	"testing"
	"time"

	"zeolite/internal/probe"
	"zeolite/internal/sandbox"
)

func gateConfig() sandbox.GatekeeperConfig {
	return sandbox.GatekeeperConfig{
		Resolver:          "1.1.1.1",
		AllowedInterfaces: []sandbox.AllowedInterface{{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7"}}},
		ProbeURLs:         []string{"https://probe.example.com/ip"},
	}
}

// outputChain is the output chain of a ruleset, one rule a line.
func outputChain(t *testing.T, rules string) []string {
	t.Helper()
	from, to := strings.Index(rules, "chain gate_output"), strings.Index(rules, "chain gate_input")
	if from < 0 || to < from {
		t.Fatalf("the ruleset has no output chain ahead of its input chain:\n%s", rules)
	}
	var lines []string
	for _, line := range strings.Split(rules[from:to], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestRuleset(t *testing.T) {
	cfg := gateConfig()
	cfg.Resolver = "9.9.9.9"
	rules, err := ruleset(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set open { typeof meta mark; flags timeout; }",
		"type filter hook output priority 0; policy drop;",
		`meta mark 0x1 oifname "tun0" ip daddr 9.9.9.9 meta l4proto { tcp, udp } th dport 53 accept`,
		`meta mark 0x1 oifname "tun0" ip daddr != @nonpublic4 tcp dport 443 accept`,
		"meta mark 0x1 counter name rejected reject",
		`oifname "tun0" ip daddr 9.9.9.9 udp dport 53 meta mark @open counter name passed accept`,
		`oifname "tun0" ip daddr != @nonpublic4 meta l4proto tcp meta mark @open counter name passed accept`,
		"meta l4proto tcp counter name rejected reject with tcp reset",
		"type filter hook input priority 0; policy drop;",
		`iifname "tun0" ip saddr != @nonpublic4 ct state established accept`,
		"type filter hook forward priority 0; policy drop;",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("the ruleset lacks %q", want)
		}
	}
	// An accept of established flows in output would keep a flow alive
	// past its lease.
	if strings.Contains(strings.Join(outputChain(t, rules), "\n"), "established") {
		t.Error("the output chain accepts established flows")
	}
	// The gatekeeper carries nothing: no packet is sent anywhere but where
	// it was going.
	for _, word := range []string{"type nat", "redirect", "dnat", "snat", "masquerade"} {
		if strings.Contains(rules, word) {
			t.Errorf("the ruleset rewrites packets: it says %q", word)
		}
	}
	// Every range the sandbox must not reach is in the set the accepts
	// are judged against.
	for _, p := range sandbox.NonPublicPrefixes {
		if p.Addr().Is4() && !strings.Contains(rules, p.String()) {
			t.Errorf("the set of non-public ranges lacks %s", p)
		}
	}
}

// Nothing leaves the namespace but by the lease or on the probe's own
// sockets, and by the TUN device: every accept in the output chain is
// for loopback, or names the device and is for a socket that carries the
// probe's mark or a lookup of the lease.
func TestRulesetPassesNothingButByTheLeaseOrTheProbe(t *testing.T) {
	rules, err := ruleset(gateConfig())
	if err != nil {
		t.Fatal(err)
	}
	accepts := 0
	for _, line := range outputChain(t, rules) {
		if !strings.HasSuffix(line, "accept") {
			continue
		}
		accepts++
		switch {
		case line == `oifname "lo" fib daddr type local accept`:
		case strings.HasPrefix(line, `meta mark 0x1 oifname "tun0" ip daddr 1.1.1.1 `) && strings.Contains(line, "th dport 53"):
		case strings.HasPrefix(line, `meta mark 0x1 oifname "tun0" ip daddr != @nonpublic4 tcp dport 443 `):
		case strings.HasPrefix(line, `oifname "tun0" `) && strings.Contains(line, "meta mark @open") && (strings.Contains(line, "ip daddr != @nonpublic4") || strings.Contains(line, "ip daddr 1.1.1.1 udp dport 53")):
		default:
			t.Errorf("the output chain accepts packets that neither the lease nor the probe's mark vouches for: %s", line)
		}
	}
	if accepts != 5 {
		t.Errorf("found %d accepts in the output chain, want 5: the test is not reading it, or a rule was added", accepts)
	}
}

// A probe pointed at the sandbox itself is rejected: the rule that ends
// the probe's packets comes before the one that accepts loopback.
func TestRulesetJudgesTheProbeAndTheLeaseBeforeLoopback(t *testing.T) {
	rules, err := ruleset(gateConfig())
	if err != nil {
		t.Fatal(err)
	}
	index := func(rule string) int {
		for i, line := range outputChain(t, rules) {
			if line == rule {
				return i
			}
		}
		t.Fatalf("the output chain lacks %q", rule)
		return -1
	}
	loopback := index(`oifname "lo" fib daddr type local accept`)
	if at := index("meta mark 0x1 counter name rejected reject"); at > loopback {
		t.Error("loopback is accepted before the probe's packets are judged: something in the sandbox could answer a probe")
	}
	// What comes in is loopback's, or the device's: the input chain names
	// no other.
	from := strings.Index(rules, "chain gate_input")
	for _, line := range strings.Split(rules[from:strings.Index(rules, "chain gate_forward")], "\n") {
		if line = strings.TrimSpace(line); strings.HasSuffix(line, "accept") && !strings.HasPrefix(line, `iifname "lo" `) && !strings.HasPrefix(line, `iifname "tun0" `) {
			t.Errorf("the input chain accepts from a device that is neither loopback nor the TUN device: %s", line)
		}
	}
}

// The Probe Source Ports are the probe's alone: what comes from one of
// them without the probe's mark is rejected, whatever the lease or the
// Hold says, since the Carrier takes a connection from one for a
// probe's. The probe's own packets were judged before, and loopback's
// accepted: a server in the sandbox may listen on any port.
func TestRulesetKeepsTheProbeSourcePortsForTheProbe(t *testing.T) {
	cfg := gateConfig()
	cfg.Hold = true
	rules, err := ruleset(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const rule = "meta l4proto { tcp, udp } th sport 61000-61999 counter name rejected reject"
	at, marked, loopback := -1, -1, -1
	for i, line := range outputChain(t, rules) {
		switch {
		case line == rule:
			at = i
		case line == "meta mark 0x1 counter name rejected reject":
			marked = i
		case line == `oifname "lo" fib daddr type local accept`:
			loopback = i
		case strings.Contains(line, "@open") || strings.Contains(line, "@held"):
			if at < 0 {
				t.Errorf("the lease or the Hold is looked up before the Probe Source Ports are judged: %s", line)
			}
		}
	}
	if at < 0 || marked < 0 || loopback < 0 || at < marked || at < loopback {
		t.Fatalf("the rule %q is line %d, the probe's own reject %d and loopback %d: it belongs after both", rule, at, marked, loopback)
	}
	if !sandbox.ProbeSourcePort(61000) || !sandbox.ProbeSourcePort(61999) || sandbox.ProbeSourcePort(60999) || sandbox.ProbeSourcePort(62000) {
		t.Error("the Probe Source Ports are not the ports the rule names")
	}
}

// A namespace that hands out one of the Probe Source Ports by itself is
// refused: a connection of the sandbox's that was given one would be
// rejected.
func TestTheNamespaceHandsOutNoProbeSourcePort(t *testing.T) {
	for printed, ok := range map[string]bool{
		"32768\t60999\n": true,
		"1024 60999":     true,
		"62000 65535":    true,
		"32768 61000":    false,
		"1024\t65535\n":  false,
		"61999 61999":    false,
		"61500 61600":    false,
		"":               false,
		"many":           false,
	} {
		if err := checkLocalPortRange(printed); (err == nil) != ok {
			t.Errorf("a namespace that hands out %q: %v", printed, err)
		}
	}
}

// The gate says how long its lease has to run: the Carrier sends on
// what the sandbox sends for that long, and no longer unasked.
func TestGateSaysHowLongItsLeaseRuns(t *testing.T) {
	g, _, now := testGate()
	g.heartbeat(beat(true))
	if tel := g.telemetry(); tel.Open || tel.LeaseFor != 0 {
		t.Fatalf("a locked gate says %+v", tel)
	}
	sent := *now
	*now = sent.Add(300 * time.Millisecond)
	g.answered(sent, 1, matched)
	if tel := g.telemetry(); !tel.Open || tel.LeaseFor != sandbox.LeaseDuration-300*time.Millisecond {
		t.Fatalf("300ms after the probe was sent the gate says %+v", tel)
	}
	*now = sent.Add(2 * time.Second)
	if got := g.telemetry().LeaseFor; got != sandbox.LeaseDuration-2*time.Second {
		t.Errorf("two seconds after the probe was sent the lease runs for %v", got)
	}
	g.answered(*now, 1, mismatched)
	if tel := g.telemetry(); tel.Open || tel.LeaseFor != 0 {
		t.Errorf("after an answer that is not expected the gate says %+v", tel)
	}
}

// The probes on their way are of one Interface in use: those of the
// same go on, and those of another, or of the same after a change, are
// ended when the next is asked for, as they are when there is none.
func TestProbesOnTheirWayEndWithTheirInterfaceInUse(t *testing.T) {
	var u underway
	first := u.of("en0", 1)
	if again := u.of("en0", 1); again != first || first.Err() != nil {
		t.Fatal("the probes of the Interface in use were ended, or are no longer the same")
	}
	none := u.of("", 2)
	if first.Err() == nil || none.Err() != nil {
		t.Fatalf("with no Interface in use, the probes of the one before go on (%v), or those of none were ended (%v)", first.Err(), none.Err())
	}
	back := u.of("en0", 3)
	if none.Err() == nil || back.Err() != nil {
		t.Fatal("the Interface in use came back, and what was on its way before goes on")
	}
	other := u.of("utun4", 3)
	if back.Err() == nil || other.Err() != nil {
		t.Fatal("another Interface is in use, and the probes of the one before go on")
	}
}

func TestRulesetRefuses(t *testing.T) {
	for _, resolver := range []string{"", "dns.example.com", "192.168.5.3", "127.0.0.11", "2606:4700:4700::1111", "1.1.1.1; accept"} {
		cfg := gateConfig()
		cfg.Resolver = resolver
		if rules, err := ruleset(cfg); err == nil {
			t.Errorf("the resolver %q got a ruleset:\n%s", resolver, rules)
		}
	}
}

func TestLeaseScript(t *testing.T) {
	lease := "flush set inet zeolite open\nadd element inet zeolite open { 0x0 timeout 4500ms }\n"
	hold := "flush set inet zeolite held\nadd element inet zeolite held { 0x0 timeout 9500ms }\n"
	for _, c := range []struct {
		lease, hold int64
		want        string
	}{
		{4500, 0, lease},
		{4500, 9500, lease + hold},
		// An answer that came too late for the lease leaves it alone.
		{0, 9500, hold},
		{0, 0, ""},
	} {
		if got := leaseScript(c.lease, c.hold); got != c.want {
			t.Errorf("the script for a lease of %dms and a Hold of %dms is %q, want %q", c.lease, c.hold, got, c.want)
		}
	}
	if got := endScript(false); got != "flush set inet zeolite open\n" {
		t.Errorf("the script that ends a lease is %q", got)
	}
	// A ruleset with no Hold has no set to flush: the script would fail.
	if got := endScript(true); got != "flush set inet zeolite open\nflush set inet zeolite held\n" {
		t.Errorf("the script that ends a lease and its Hold is %q", got)
	}
}

func TestRulesetIdentityIgnoresTheLease(t *testing.T) {
	locked := "table inet zeolite {\n\tset open {\n\t\ttypeof meta mark\n\t\tflags timeout\n\t}\n\n\tset nonpublic4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\telements = { 10.0.0.0/8, 127.0.0.0/8 }\n\t}\n}\n"
	open := strings.Replace(locked, "flags timeout\n", "flags timeout\n\t\telements = { 0x00000000 timeout 5s }\n", 1)
	if open == locked {
		t.Fatal("the test's listings do not differ")
	}
	if rulesetIdentity(open) != rulesetIdentity(locked) {
		t.Errorf("an open gate and a locked one have different identities:\n%s", rulesetIdentity(open))
	}
	// The other sets' elements are part of the ruleset.
	if !strings.Contains(rulesetIdentity(open), "10.0.0.0/8") {
		t.Error("the identity lost a set that is not the lease")
	}
	tampered := strings.Replace(locked, "10.0.0.0/8, ", "", 1)
	if rulesetIdentity(tampered) == rulesetIdentity(locked) {
		t.Error("a changed set does not change the identity")
	}
	// The Hold comes and goes as the lease does.
	withHold := strings.Replace(locked, "\tset nonpublic4", "\tset held {\n\t\ttypeof meta mark\n\t\tflags timeout\n\t}\n\n\tset nonpublic4", 1)
	holding := strings.Replace(withHold, "set held {\n\t\ttypeof meta mark\n\t\tflags timeout\n", "set held {\n\t\ttypeof meta mark\n\t\tflags timeout\n\t\telements = { 0x00000000 timeout 10s expires 7s }\n", 1)
	if holding == withHold || withHold == locked {
		t.Fatal("the test's listings with a Hold do not differ")
	}
	if rulesetIdentity(holding) != rulesetIdentity(withHold) {
		t.Errorf("a gate that holds and one that does not have different identities:\n%s", rulesetIdentity(holding))
	}
	if rulesetIdentity(withHold) == rulesetIdentity(locked) {
		t.Error("a ruleset with a Hold has the identity of one without")
	}
}

func TestParseCounters(t *testing.T) {
	out := `{"nftables": [{"metainfo": {"version": "1.0.6"}}, {"counter": {"family": "inet", "name": "passed", "table": "zeolite", "handle": 1, "packets": 42, "bytes": 9000}}, {"counter": {"family": "inet", "name": "rejected", "table": "zeolite", "handle": 2, "packets": 7, "bytes": 420}}]}`
	passed, rejected, err := parseCounters([]byte(out))
	if err != nil || passed != 42 || rejected != 7 {
		t.Errorf("parsed %d passed, %d rejected, error %v", passed, rejected, err)
	}
}

// fakeLease records what the gate orders.
type fakeLease struct {
	held bool
	ttl  time.Duration
	fail error
	// failEnd is what ending the lease answers, and ended how often it
	// was ended.
	failEnd error
	ended   int
	// renewed is how often it was renewed.
	renewed int
	// holding is whether a Hold was renewed and not ended since, holdTTL
	// for how long it last was, and holds how often.
	holding bool
	holdTTL time.Duration
	holds   int
	// standIns is whether the stand-in addresses are reached, as the
	// lease was last ended for an Interface in use, and turned how often
	// it was ended so.
	standIns bool
	turned   int
}

func (f *fakeLease) renew(lease, hold time.Duration) error {
	if f.fail != nil {
		return f.fail
	}
	if lease > 0 {
		f.held, f.ttl = true, lease
		f.renewed++
	}
	if hold > 0 {
		f.holding, f.holdTTL = true, hold
		f.holds++
	}
	return nil
}

func (f *fakeLease) end() error {
	if f.failEnd != nil {
		return f.failEnd
	}
	f.held, f.holding = false, false
	f.ended++
	return nil
}

func (f *fakeLease) endFor(standIns bool) error {
	if err := f.end(); err != nil {
		return err
	}
	f.standIns = standIns
	f.turned++
	return nil
}

// beat is a heartbeat as the app sends one while the sandbox leaves by
// en0, the first Interface in use it ever had.
func beat(met bool) sandbox.Heartbeat {
	return sandbox.Heartbeat{HostConditionsMet: met, Interface: "en0", InterfaceChange: 1}
}

func testGate() (*gate, *fakeLease, *time.Time) {
	l := &fakeLease{}
	now := time.Unix(1000, 0)
	g := newGate(l, func(error) {})
	g.now = func() time.Time { return now }
	return g, l, &now
}

// What a probe URL answers: the address expected at en0, and another.
var (
	matched    = probeAnswer{exit: "203.0.113.7", matches: true, latency: 80 * time.Millisecond}
	mismatched = probeAnswer{exit: "198.51.100.9", latency: 90 * time.Millisecond}
)

func TestGate(t *testing.T) {
	g, held, now := testGate()

	// A matching answer does not open the gate before the app is heard.
	g.answered(*now, 0, matched)
	if held.held || g.telemetry().LockReason != sandbox.LockHeartbeatLost {
		t.Fatalf("open %v, reason %q before any heartbeat", held.held, g.telemetry().LockReason)
	}

	// A heartbeat alone does not open it either: an answer does.
	g.heartbeat(beat(true))
	if held.held {
		t.Fatal("a heartbeat opened the gate")
	}

	// The lease is counted from when the probe was sent.
	start := *now
	*now = now.Add(800 * time.Millisecond)
	g.answered(start, 1, matched)
	if !held.held || held.ttl != sandbox.LeaseDuration-800*time.Millisecond {
		t.Fatalf("open %v, ttl %v", held.held, held.ttl)
	}
	if tel := g.telemetry(); !tel.Open || tel.ExitAddress != "203.0.113.7" || tel.LockReason != "" || tel.ProbeLatency != matched.latency {
		t.Errorf("telemetry %+v", tel)
	}

	// The Host Conditions fail: locked at once, before an answer.
	g.heartbeat(beat(false))
	if held.held || g.telemetry().LockReason != sandbox.LockHostCondition {
		t.Fatalf("open %v, reason %q", held.held, g.telemetry().LockReason)
	}
	g.answered(*now, 1, matched)
	if held.held {
		t.Fatal("an answer opened the gate while a Host Condition was unmet")
	}

	// Heartbeats that stop are noticed by the next answer.
	g.heartbeat(beat(true))
	g.answered(*now, 1, matched)
	if !held.held {
		t.Fatal("the gate stayed locked once the Host Conditions held again")
	}
	*now = now.Add(sandbox.LeaseDuration + time.Second)
	g.answered(*now, 1, matched)
	if held.held || g.telemetry().LockReason != sandbox.LockHeartbeatLost {
		t.Fatalf("open %v, reason %q after the heartbeats stopped", held.held, g.telemetry().LockReason)
	}

	// An answer that took longer than the lease proves nothing about now.
	g.heartbeat(beat(true))
	start = *now
	*now = now.Add(sandbox.LeaseDuration)
	g.heartbeat(beat(true))
	g.answered(start, 1, matched)
	if held.held {
		t.Fatal("an answer older than the lease opened the gate")
	}

	// The end of the stream locks for good.
	g.answered(*now, 1, matched)
	if !held.held {
		t.Fatal("a matching answer did not open the gate")
	}
	g.heartbeatEnd()
	g.answered(*now, 1, matched)
	if held.held || g.telemetry().Open {
		t.Fatal("the gate opened after the app's stream ended")
	}
}

// A probe nobody answers says nothing. The lease an answer earned runs
// its time, counted from when that probe was sent, and the gate locks
// when it is over.
func TestGateLetsTheLeaseRunOutWhenNothingAnswers(t *testing.T) {
	g, held, now := testGate()
	told := 0
	g.changed = func() { told++ }
	var due time.Duration
	g.renewed = func(d time.Duration) { due = d }

	g.heartbeat(beat(true))
	sent := *now
	*now = now.Add(300 * time.Millisecond)
	g.answered(sent, 1, matched)
	if want := sandbox.LeaseDuration - 300*time.Millisecond; !held.held || due != want {
		t.Fatalf("lease held %v, its end due in %v, want %v", held.held, due, want)
	}

	// Four rounds go unanswered: the gate is told nothing of them, and
	// the lease is neither ended nor renewed.
	ended, renewed, before := held.ended, held.renewed, told
	for range 4 {
		*now = now.Add(time.Second)
		g.heartbeat(beat(true))
		g.lapse()
	}
	if !held.held || !g.telemetry().Open || held.ended != ended || held.renewed != renewed || told != before {
		t.Fatalf("with rounds unanswered inside the lease: held %v, ended %d times, renewed %d times, told %d times", held.held, held.ended-ended, held.renewed-renewed, told-before)
	}

	// The lease is over: locked, and said at once.
	*now = sent.Add(sandbox.LeaseDuration)
	g.heartbeat(beat(true))
	g.lapse()
	if tel := g.telemetry(); held.held || tel.Open || tel.LockReason != sandbox.LockProbeUnanswered || told != before+1 {
		t.Fatalf("when the lease was over: held %v, telemetry %+v, told %d times", held.held, tel, told-before)
	}

	// An answer that was on its way opens the gate again, for a lease's
	// duration from when its probe was sent.
	slow := sent.Add(4 * time.Second)
	*now = sent.Add(6500 * time.Millisecond)
	g.heartbeat(beat(true))
	g.answered(slow, 1, matched)
	if want := 2500 * time.Millisecond; !held.held || held.ttl != want || !g.telemetry().Open {
		t.Fatalf("a slow answer: held %v for %v, want %v", held.held, held.ttl, want)
	}

	// The heartbeats stop under an open gate, and nothing answers: the
	// gate says why it is locked.
	*now = now.Add(sandbox.LeaseDuration + time.Second)
	g.lapse()
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockHeartbeatLost {
		t.Fatalf("after the heartbeats stopped: held %v, reason %q", held.held, tel.LockReason)
	}
}

// Of the answers to the probes sent together the first renews the lease.
// The others, and a late answer to an earlier probe, neither renew it
// again nor shorten it.
func TestGateRenewsToTheLatestProbeAnswered(t *testing.T) {
	g, held, now := testGate()
	g.heartbeat(beat(true))
	first := *now
	*now = now.Add(time.Second)
	second := *now
	*now = now.Add(200 * time.Millisecond)

	fastest := probeAnswer{exit: "203.0.113.7", matches: true, latency: 200 * time.Millisecond}
	g.answered(second, 1, fastest)
	if want := sandbox.LeaseDuration - 200*time.Millisecond; held.renewed != 1 || held.ttl != want {
		t.Fatalf("the first answer renewed the lease %d times, for %v, want once for %v", held.renewed, held.ttl, want)
	}
	*now = now.Add(300 * time.Millisecond)
	g.answered(second, 1, matched) // another probe URL, the same round
	g.answered(first, 1, matched)  // the round before, late
	if held.renewed != 1 || held.ttl != sandbox.LeaseDuration-200*time.Millisecond {
		t.Errorf("answers with nothing to add renewed the lease %d more times, to %v", held.renewed-1, held.ttl)
	}
	// What is shown is the first answer to the latest probe answered.
	if tel := g.telemetry(); tel.ProbeLatency != fastest.latency || !tel.Open {
		t.Errorf("telemetry %+v, want the latency of the round's first answer", tel)
	}
	// The next round's first answer renews it.
	*now = second.Add(time.Second + 50*time.Millisecond)
	g.answered(second.Add(time.Second), 1, matched)
	if held.renewed != 2 || held.ttl != sandbox.LeaseDuration-50*time.Millisecond {
		t.Errorf("the next round renewed the lease %d times, to %v", held.renewed-1, held.ttl)
	}
}

// An answer with an address that is not expected locks the gate at
// once, and is believed for a lease's duration from when its probe was
// sent: until then no answer opens the gate.
func TestGateLocksOnAnAddressThatIsNotExpected(t *testing.T) {
	g, held, now := testGate()
	told := 0
	g.changed = func() { told++ }
	g.heartbeat(beat(true))
	g.answered(*now, 1, matched)

	sent := *now
	*now = now.Add(100 * time.Millisecond)
	ended, before := held.ended, told
	g.answered(sent, 1, mismatched)
	if held.held || held.ended != ended+1 || told != before+1 {
		t.Fatalf("after a mismatch: lease held %v, ended %d times, told %d times", held.held, held.ended-ended, told-before)
	}
	if tel := g.telemetry(); tel.Open || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "198.51.100.9" {
		t.Errorf("telemetry %+v", tel)
	}

	// Another probe URL of the same round answers as expected, and so
	// does the next round: neither opens the gate, and the address shown
	// is the one that was not expected.
	g.answered(sent, 1, matched)
	*now = now.Add(time.Second)
	g.heartbeat(beat(true))
	g.lapse()
	g.answered(*now, 1, matched)
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "198.51.100.9" {
		t.Fatalf("while a mismatch is believed: lease held %v, telemetry %+v", held.held, tel)
	}
	// A lease that is ended is not ended again.
	if held.ended != ended+1 {
		t.Errorf("the lease was ended %d times for one lock", held.ended-ended)
	}

	// A Host Condition that fails besides is the weightier reason.
	g.heartbeat(beat(false))
	g.answered(*now, 1, mismatched)
	if got := g.telemetry().LockReason; got != sandbox.LockHostCondition {
		t.Errorf("with a Host Condition unmet and a mismatch the gate says %q", got)
	}
	g.heartbeat(beat(true))

	// A lease's duration after the last probe so answered was sent, the
	// mismatch says nothing of now: the gate is locked for want of an
	// answer, and the next that matches opens it.
	*now = now.Add(sandbox.LeaseDuration)
	g.heartbeat(beat(true))
	g.lapse()
	if got := g.telemetry().LockReason; got != sandbox.LockProbeUnanswered {
		t.Errorf("once the mismatch was over the gate says %q", got)
	}
	g.answered(*now, 1, matched)
	if tel := g.telemetry(); !held.held || !tel.Open || tel.ExitAddress != "203.0.113.7" {
		t.Fatalf("once the mismatch was over: lease held %v, telemetry %+v", held.held, tel)
	}

	// An answer older than a lease lasts says nothing, whatever it says.
	old := *now
	*now = now.Add(sandbox.LeaseDuration)
	g.heartbeat(beat(true))
	g.answered(*now, 1, matched)
	g.answered(old, 1, mismatched)
	if !held.held || !g.telemetry().Open {
		t.Fatal("a mismatch older than a lease lasts locked the gate")
	}

	// The first match renews the lease, and shields the gate from no other
	// answer: a mismatch to an earlier round that arrives after a later
	// round's match locks the gate when it arrives, and is believed until
	// a lease's duration after its own probe was sent.
	earlier := *now
	*now = now.Add(2 * time.Second)
	g.heartbeat(beat(true))
	later := *now
	*now = now.Add(100 * time.Millisecond)
	g.answered(later, 1, matched)
	if !held.held || !g.telemetry().Open {
		t.Fatal("the later round's match did not keep the gate open")
	}
	*now = now.Add(100 * time.Millisecond)
	g.answered(earlier, 1, mismatched)
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "198.51.100.9" {
		t.Fatalf("a mismatch to the round before, after this round's match: lease held %v, telemetry %+v", held.held, tel)
	}
	g.answered(later, 1, matched)
	*now = earlier.Add(sandbox.LeaseDuration - time.Millisecond)
	g.heartbeat(beat(true))
	g.answered(*now, 1, matched)
	if held.held {
		t.Error("a match opened the gate while the earlier round's mismatch was believed")
	}
	*now = earlier.Add(sandbox.LeaseDuration)
	g.heartbeat(beat(true))
	g.answered(*now, 1, matched)
	if !held.held {
		t.Error("the gate stayed locked once the earlier round's mismatch was over")
	}
}

// The next round is sent two seconds after one that was answered, and a
// second after one that nothing has answered yet.
func TestPace(t *testing.T) {
	if unansweredInterval != tick || probeInterval != 2*unansweredInterval {
		t.Fatalf("a round's time is two of the intervals after which an answer is late, and that one a tick of %v: they are %v and %v", tick, probeInterval, unansweredInterval)
	}
	// Two rounds lost after a probe that was answered leave a third the
	// time to be answered before that one's lease runs out.
	if probeInterval+2*unansweredInterval >= sandbox.LeaseDuration {
		t.Fatalf("a round every %v, and every %v while unanswered, does not survive two lost rounds in a lease of %v", probeInterval, unansweredInterval, sandbox.LeaseDuration)
	}

	// run asks a pace at each of so many slots, a little early or late
	// as a ticker comes, and answers each probe so long after it was
	// sent, or never. It returns which probe URLs were asked at each
	// slot.
	jitter := []time.Duration{0, 3 * time.Millisecond, -2 * time.Millisecond, 20 * time.Millisecond, -15 * time.Millisecond}
	at := time.Unix(1000, 0)
	run := func(p *pace, slots int, answeredAfter time.Duration, change uint64) [][]int {
		asked := make([][]int, slots)
		var sent []time.Time
		for i := range slots {
			now := at.Add(time.Duration(i)*p.every() + jitter[i%len(jitter)])
			for answeredAfter >= 0 && len(sent) > 0 && !sent[0].Add(answeredAfter).After(now) {
				p.answer(sent[0])
				sent = sent[1:]
			}
			asked[i] = p.due(now, change)
			for range asked[i] {
				sent = append(sent, now)
			}
		}
		at = at.Add(time.Duration(slots) * p.every())
		return asked
	}
	says := func(asked [][]int) string { return strings.ReplaceAll(fmt.Sprint(asked), "] [", "][") }
	count := func(asked [][]int) (n int) {
		for _, slot := range asked {
			n += len(slot)
		}
		return n
	}
	const never = -1

	// Three probe URLs: a slot is a third of a second, and six are a
	// round's time.
	p := newPace(3)
	if p.every() != unansweredInterval/3 {
		t.Fatalf("with three probe URLs a slot lasts %v", p.every())
	}
	// The first round is sent together. Answered at once, each probe URL
	// is then asked every two seconds, and one of them every two thirds
	// of a second.
	if got, want := says(run(p, 13, 100*time.Millisecond, 1)), "[[0 1 2][][1][][2][][0][][1][][2][][0]]"; got != want {
		t.Errorf("answered at once, three probe URLs were asked %s, want %s", got, want)
	}
	// Nothing answers: from a second after the oldest probe that waits,
	// each is asked every second, and one of them every third of one.
	p = newPace(3)
	if got, want := says(run(p, 12, never, 1)), "[[0 1 2][][1][0][2][1][0][2][1][0][2][1]]"; got != want {
		t.Errorf("unanswered, three probe URLs were asked %s, want %s", got, want)
	}
	// What waits is counted only as far as it says that the answers are
	// late.
	run(p, 600, never, 1)
	if len(p.waiting) > 33 {
		t.Errorf("after ten minutes with no answer the pace remembers %d probes", len(p.waiting))
	}
	// Answers that take less than a second leave the pace as it is, and
	// answers that take more double it, as many probes as there are
	// slots: the same number of requests as when a round was sent
	// together.
	for _, c := range []struct {
		after time.Duration
		want  int
	}{{300 * time.Millisecond, 30}, {900 * time.Millisecond, 30}, {1200 * time.Millisecond, 60}, {4 * time.Second, 60}} {
		p = newPace(3)
		if got := count(run(p, 90, c.after, 1)[30:]); got != c.want {
			t.Errorf("with answers after %v, %d probes were sent in twenty seconds, want %d", c.after, got, c.want)
		}
	}
	// The answers come again in time: the pace is the slower one again.
	run(p, 30, 100*time.Millisecond, 1)
	if got := count(run(p, 60, 100*time.Millisecond, 1)); got != 30 {
		t.Errorf("once the answers came in time again, %d probes were sent in twenty seconds, want 30", got)
	}

	// Another Interface in use: what was asked of the one before says
	// nothing, and its first round is sent at once, together.
	if got, want := says(run(p, 5, 100*time.Millisecond, 2)), "[[0 1 2][][1][][2]]"; got != want {
		t.Errorf("after the Interface in use changed, three probe URLs were asked %s, want %s", got, want)
	}

	// One probe URL: a slot is a second, and it is asked every two, or
	// every one while nothing answers.
	p = newPace(1)
	if got, want := says(run(p, 5, 100*time.Millisecond, 1)), "[[0][][0][][0]]"; got != want || p.every() != unansweredInterval {
		t.Errorf("answered at once, one probe URL was asked %s every %v, want %s", got, p.every(), want)
	}
	p = newPace(1)
	if got, want := says(run(p, 5, never, 1)), "[[0][0][0][0][0]]"; got != want {
		t.Errorf("unanswered, one probe URL was asked %s, want %s", got, want)
	}

	// More probe URLs than a round is spread over: a slot is no shorter
	// than a tenth of a second, some are asked together, and each still
	// once in two seconds.
	p = newPace(12)
	run(p, 20, 50*time.Millisecond, 1)
	asked := run(p, 20, 50*time.Millisecond, 1)
	if p.every() != unansweredInterval/maxSpread || count(asked) != 12 {
		t.Errorf("twelve probe URLs were asked %s in two seconds, a slot being %v", says(asked), p.every())
	}
}

func TestGateLocksWhenTheLeaseIsRefused(t *testing.T) {
	g, held, now := testGate()
	g.heartbeat(beat(true))
	held.fail = errors.New("nft: no such table")
	g.answered(*now, 1, matched)
	if g.telemetry().Open {
		t.Fatal("the gate reports open although the kernel refused the lease")
	}
}

// answers stands in for the probe's client: what each URL answers, by
// its path.
type answers map[string]string

func (a answers) Fetch(ctx context.Context, url string) (string, error) {
	if said, asked := a[reachedStandIns]; asked {
		a[reachedStandIns] = said + fmt.Sprintf("%s %v\n", url, probe.ReachesStandIns(ctx))
	}
	body, ok := a[url]
	if !ok {
		return "", errors.New("the endpoint answered 404 Not Found")
	}
	return strings.TrimSpace(body), nil
}

func (a answers) ExitAddress(ctx context.Context, url string) (netip.Addr, error) {
	body, err := a.Fetch(ctx, url)
	if err != nil {
		return netip.Addr{}, err
	}
	return sandbox.ParseExitAddress(body)
}

// probeURL is the probe URL of gateConfig, and timezoneURL where the
// tests' timezone is asked. Under reachedStandIns, where a test put
// something there, each probe is noted with whether it said that it
// reaches stand-in addresses.
const (
	probeURL        = "https://probe.example.com/ip"
	timezoneURL     = "https://probe.example.com/timezone"
	reachedStandIns = "reached stand-in addresses"
)

// proberOf makes a prober of gateConfig, changed, whose probes are
// answered as answers says.
func proberOf(t *testing.T, a answers, change func(*sandbox.GatekeeperConfig)) *prober {
	t.Helper()
	cfg := gateConfig()
	if change != nil {
		change(&cfg)
	}
	p, err := newProber(cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The request itself is internal/probe's, and tested there. Here an
// answer is judged.
func TestProbe(t *testing.T) {
	a := answers{probeURL: "203.0.113.7\n", timezoneURL: "Europe/Berlin\n"}
	ctx := context.Background()
	p := proberOf(t, a, nil)
	ask := func() (probeAnswer, error) { return p.ask(ctx, p.urls[0], "en0") }

	if got, err := ask(); err != nil || !got.matches || got.exit != "203.0.113.7" || got.latency <= 0 {
		t.Fatalf("a probe answered with the expected address gave %+v, %v", got, err)
	}
	a[probeURL] = "198.51.100.9"
	if got, err := ask(); err != nil || got.matches || got.exit != "198.51.100.9" {
		t.Errorf("a probe answered with another address gave %+v, %v", got, err)
	}
	// An address the sandbox cannot leave from is a mismatch like another.
	a[probeURL] = "2001:db8::7"
	if got, err := ask(); err != nil || got.matches || got.exit != "2001:db8::7" {
		t.Errorf("a probe answered with an IPv6 address gave %+v, %v", got, err)
	}
	// What is no address is no answer, and neither is no answer at all.
	a[probeURL] = "not an address"
	if got, err := ask(); err == nil {
		t.Errorf("a probe answered with what is no address gave %+v", got)
	}
	delete(a, probeURL)
	if got, err := ask(); err == nil || !strings.Contains(err.Error(), probeURL) {
		t.Errorf("a probe nobody answered gave %+v, %v", got, err)
	}

	if tz, err := p.timezone(ctx, timezoneURL, "en0"); err != nil || tz != "Europe/Berlin" {
		t.Errorf("the timezone probe gave %q, %v", tz, err)
	}
	a[timezoneURL] = "<html>"
	if tz, err := p.timezone(ctx, timezoneURL, "en0"); err == nil {
		t.Errorf("a timezone probe answered with markup gave %q", tz)
	}
}

func TestProberRefusesAConfigItCannotJudge(t *testing.T) {
	for what, change := range map[string]func(*sandbox.GatekeeperConfig){
		"no probe URL":             func(c *sandbox.GatekeeperConfig) { c.ProbeURLs = nil },
		"no Allowed Interface":     func(c *sandbox.GatekeeperConfig) { c.AllowedInterfaces = nil },
		"no expected exit address": func(c *sandbox.GatekeeperConfig) { c.AllowedInterfaces[0].ExpectedExitAddresses = nil },
		"an address and any":       func(c *sandbox.GatekeeperConfig) { c.AllowedInterfaces[0].AnyExitAddress = true },
		"an address and any but physical": func(c *sandbox.GatekeeperConfig) {
			c.AllowedInterfaces[0].AnyExitAddressButPhysical = true
		},
		"any and any but physical": func(c *sandbox.GatekeeperConfig) {
			c.AllowedInterfaces[0] = sandbox.AllowedInterface{Name: "en0", AnyExitAddress: true, AnyExitAddressButPhysical: true}
		},
		"physical beside addresses": func(c *sandbox.GatekeeperConfig) {
			c.AllowedInterfaces[0].PhysicalExitAddress = true
		},
		"physical beside any but physical": func(c *sandbox.GatekeeperConfig) {
			c.AllowedInterfaces[0] = sandbox.AllowedInterface{Name: "en0", PhysicalExitAddress: true, AnyExitAddressButPhysical: true}
		},
		"an Interface listed twice": func(c *sandbox.GatekeeperConfig) {
			c.AllowedInterfaces = append(c.AllowedInterfaces, c.AllowedInterfaces[0])
		},
		"an Interface that is no name": func(c *sandbox.GatekeeperConfig) { c.AllowedInterfaces[0].Name = "en 0" },
		"a private exit address": func(c *sandbox.GatekeeperConfig) {
			c.AllowedInterfaces[0].ExpectedExitAddresses = []string{"192.168.1.7"}
		},
		"an exit address with a space": func(c *sandbox.GatekeeperConfig) {
			c.AllowedInterfaces[0].ExpectedExitAddresses = []string{"203.0.113.7 x"}
		},
	} {
		cfg := gateConfig()
		change(&cfg)
		if _, err := newProber(cfg, answers{}); err == nil {
			t.Errorf("%s got a prober", what)
		}
	}
}

// listens reports the selectors in a file that open a listening socket,
// take a connection or serve one.
func listens(file *ast.File) (found []*ast.SelectorExpr, selectors int) {
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		selectors++
		for _, prefix := range []string{"Listen", "Accept", "Serve", "FileListener", "FilePacketConn"} {
			if strings.HasPrefix(sel.Sel.Name, prefix) {
				found = append(found, sel)
			}
		}
		return true
	})
	return found, selectors
}

// The gatekeeper carries nothing of the sandbox's and listens on
// nothing: no file of the package, or of the one that makes its probe's
// request, opens a listening socket or serves.
func TestTheGatekeeperListensOnNothing(t *testing.T) {
	fset := token.NewFileSet()
	files, selectors := 0, 0
	// Its own package, and the one that makes its probe's request.
	for _, dir := range []string{".", "../../internal/probe"} {
		pkgs, err := parser.ParseDir(fset, dir, func(info fs.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				files++
				found, n := listens(file)
				selectors += n
				for _, sel := range found {
					t.Errorf("%s: the gatekeeper uses %s", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
		}
	}
	if files < 6 || selectors < 100 {
		t.Errorf("read %d files and %d selectors: the test is not reading the package", files, selectors)
	}

	// The guard must be able to fail: it finds a listener where there is one.
	file, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\nimport \"net\"\nfunc f() { ln, _ := net.Listen(\"tcp4\", \"127.0.0.1:53\"); ln.Accept() }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if found, _ := listens(file); len(found) != 2 {
		t.Errorf("code that listens and accepts shows %d such selectors, want 2", len(found))
	}
}

// A heartbeat that names another Interface in use ends the lease before
// the gate takes note of it: nothing crosses the new Interface under the
// lease that was kept for the one before.
func TestGateEndsTheLeaseWhenTheInterfaceInUseChanges(t *testing.T) {
	g, held, now := testGate()
	told := 0
	g.changed = func() { told++ }

	// The first Interface in use: taken note of at once, and said.
	g.heartbeat(beat(true))
	if tel := g.telemetry(); tel.Interface != "en0" || tel.InterfaceChange != 1 || tel.Open || told != 1 {
		t.Fatalf("after the first heartbeat the gate says %+v, told %d times", tel, told)
	}
	// The gate opens, and that is said at once too.
	g.answered(*now, 1, matched)
	if !held.held || told != 2 {
		t.Fatalf("a matching probe of the Interface in use: lease held %v, told %d times", held.held, told-1)
	}
	// A heartbeat that changes nothing says nothing.
	g.heartbeat(beat(true))
	if told != 2 || !held.held {
		t.Fatalf("a heartbeat that changed nothing was told %d times, lease held %v", told-2, held.held)
	}

	// Another Interface: the lease ends first, and what the probe saw by
	// the one before is forgotten.
	probing := *now // a probe of en0 begins
	ended := held.ended
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: "utun4", InterfaceChange: 2})
	tel := g.telemetry()
	if held.held || held.ended != ended+1 || tel.Open || tel.Interface != "utun4" || tel.InterfaceChange != 2 || tel.ExitAddress != "" || told != 3 {
		t.Fatalf("after the Interface in use changed: lease held %v, ended %d times, telemetry %+v, told %d times", held.held, held.ended-ended, tel, told)
	}
	// The probe that began before the change says nothing of this one.
	g.answered(probing, 1, matched)
	if held.held || g.telemetry().Open {
		t.Fatal("a probe of the Interface before opened the gate for the one in use")
	}
	// Nor does an address that was not expected there lock this one.
	g.answered(probing, 1, mismatched)
	g.answered(*now, 2, matched)
	if !held.held || !g.telemetry().Open {
		t.Fatal("a matching probe of the new Interface in use did not open the gate")
	}

	// The kernel refuses to end the lease: the gate takes no note of the
	// change, renews nothing, and tries again at the next heartbeat.
	held.failEnd = errors.New("nft: busy")
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: "en0", InterfaceChange: 3})
	if tel := g.telemetry(); tel.InterfaceChange != 2 || tel.Interface != "utun4" || tel.Open {
		t.Fatalf("a change whose lease could not be ended was taken note of: %+v", tel)
	}
	for _, change := range []uint64{2, 3} {
		g.answered(*now, change, matched)
		g.lapse()
		if tel := g.telemetry(); tel.Open || tel.InterfaceChange != 2 {
			t.Fatalf("a probe (of change %d) left the gate saying %+v while a change waited for the lease to end", change, tel)
		}
	}
	held.failEnd = nil
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: "en0", InterfaceChange: 3})
	if tel := g.telemetry(); tel.InterfaceChange != 3 || tel.Interface != "en0" {
		t.Fatalf("once the lease could be ended the change was not taken note of: %+v", tel)
	}

	// No Interface in use: locked for it, whatever a probe says.
	g.answered(*now, 3, matched)
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, InterfaceChange: 4})
	if tel := g.telemetry(); held.held || tel.Open || tel.LockReason != sandbox.LockNoInterface || tel.Interface != "" || tel.InterfaceChange != 4 {
		t.Fatalf("with no Interface in use: lease held %v, telemetry %+v", held.held, tel)
	}
	g.answered(*now, 4, matched)
	g.lapse()
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockNoInterface || tel.ExitAddress != "" {
		t.Fatalf("with no Interface in use a probe left: lease held %v, telemetry %+v", held.held, tel)
	}
	// With none, and a Host Condition unmet besides, the Interface is
	// what is said.
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: false, InterfaceChange: 4})
	if tel := g.telemetry(); tel.LockReason != sandbox.LockNoInterface {
		t.Errorf("with no Interface in use and a Host Condition unmet the gate says %q", tel.LockReason)
	}

	// A lock that a heartbeat decides is said at once.
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: "en0", InterfaceChange: 5})
	g.answered(*now, 5, matched)
	before := told
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: false, Interface: "en0", InterfaceChange: 5})
	if told != before+1 || g.telemetry().LockReason != sandbox.LockHostCondition {
		t.Errorf("a Host Condition that failed was told %d times, as %q", told-before, g.telemetry().LockReason)
	}
}

// What the probe may see is said for each Interface, and "any" only
// where it is said.
func TestTheProbeIsJudgedByTheInterfaceInUse(t *testing.T) {
	a := answers{probeURL: "203.0.113.7"}
	ctx := context.Background()
	p := proberOf(t, a, func(c *sandbox.GatekeeperConfig) {
		c.AllowedInterfaces = []sandbox.AllowedInterface{
			{Name: "utun4", ExpectedExitAddresses: []string{"198.51.100.9", "198.51.100.10"}},
			{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7"}},
			{Name: "en7", AnyExitAddress: true},
			{Name: "utun9", AnyExitAddressButPhysical: true},
			{Name: "en5", PhysicalExitAddress: true},
		}
	})
	if got := p.butPhysical(); len(got) != 1 || !got["utun9"] {
		t.Errorf("the Interfaces that expect any but physical are %v", got)
	}
	if got := p.onlyPhysical(); len(got) != 1 || !got["en5"] {
		t.Errorf("the Interfaces that expect this Mac's physical exit address are %v", got)
	}
	// The probe is answered 203.0.113.7: expected at en0, at en7 as anything
	// is, and not at utun4.
	for iface, want := range map[string]bool{
		"en0":   true,
		"en7":   true,
		"utun4": false,
		// Any IPv4 address, as far as the config says: the gate holds it
		// against this Mac's physical exit address.
		"utun9": true,
		"en5":   true,
		// An Interface the config does not name expects nothing.
		"en9": false,
		"":    false,
	} {
		if got, err := p.ask(ctx, p.urls[0], iface); err != nil || got.matches != want || got.exit != "203.0.113.7" {
			t.Errorf("by %q the probe is judged %+v (%v), want it expected: %v", iface, got, err, want)
		}
	}
	a[probeURL] = "198.51.100.10"
	for iface, want := range map[string]bool{"utun4": true, "en7": true, "en0": false, "utun9": true} {
		if got, err := p.ask(ctx, p.urls[0], iface); err != nil || got.matches != want {
			t.Errorf("by %q an answer of 198.51.100.10 is judged %+v (%v), want it expected: %v", iface, got, err, want)
		}
	}
	// This Mac's physical exit addresses are read over IPv4: an answer of
	// another family cannot be held against them. It is expected only
	// where anything is.
	a[probeURL] = "2001:db8::7"
	for iface, want := range map[string]bool{"en7": true, "utun9": false, "en5": false, "utun4": false} {
		if got, err := p.ask(ctx, p.urls[0], iface); err != nil || got.matches != want || got.exit != "2001:db8::7" {
			t.Errorf("by %q an answer over IPv6 is judged %+v (%v), want it expected: %v", iface, got, err, want)
		}
	}
}

// tunnel is the Interface of the tests below, which expects any exit
// address but a physical one.
const tunnel = "utun9"

// tunnelGate is a gate whose Interface in use expects any exit address
// but a physical one.
func tunnelGate() (*gate, *fakeLease, *time.Time) {
	g, held, now := testGate()
	g.butPhysical = map[string]bool{tunnel: true}
	return g, held, now
}

// tunnelBeat is a heartbeat while the sandbox leaves by the tunnel, with
// this Mac's physical exit addresses known to be physical.
func tunnelBeat(physical ...string) sandbox.Heartbeat {
	return sandbox.Heartbeat{HostConditionsMet: true, Interface: tunnel, InterfaceChange: 1, PhysicalExitAddresses: physical, PhysicalExitKnown: true}
}

// What a probe answers by the tunnel: the address the tunnel leaves
// from, and this Mac's physical exit address.
var (
	byTunnel = probeAnswer{exit: "198.51.100.9", matches: true, latency: 300 * time.Millisecond}
	direct   = probeAnswer{exit: "203.0.113.7", matches: true, latency: 40 * time.Millisecond}
)

// While the app does not say that it knows this Mac's physical
// exit addresses, a gate that expects any but those is locked:
// at the heartbeat, and whatever a probe answers.
func TestGateIsLockedWhileThisMacsPhysicalExitAddressesAreNotKnown(t *testing.T) {
	g, held, now := tunnelGate()
	told := 0
	g.changed = func() { told++ }

	// A heartbeat as the app sent them before it knew of any of this:
	// not known.
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: tunnel, InterfaceChange: 1})
	if tel := g.telemetry(); tel.Open || tel.LockReason != sandbox.LockPhysicalExitUnknown || tel.PhysicalExitKnown {
		t.Fatalf("with this Mac's physical exit addresses unknown the gate says %+v", tel)
	}
	g.answered(*now, 1, byTunnel)
	g.lapse()
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockPhysicalExitUnknown {
		t.Fatalf("an answer while they were unknown: lease held %v, telemetry %+v", held.held, tel)
	}

	// Known, with none: this Mac has no other way out. The next answer
	// opens the gate, and what was known is reported.
	g.heartbeat(tunnelBeat())
	if held.held {
		t.Fatal("a heartbeat opened the gate")
	}
	g.answered(*now, 1, byTunnel)
	if tel := g.telemetry(); !held.held || !tel.Open || tel.ExitAddress != "198.51.100.9" || !tel.PhysicalExitKnown || len(tel.PhysicalExitAddresses) != 0 {
		t.Fatalf("once they were known: lease held %v, telemetry %+v", held.held, tel)
	}
	g.heartbeat(tunnelBeat("203.0.113.7", "203.0.113.8"))
	if tel := g.telemetry(); !tel.Open || strings.Join(tel.PhysicalExitAddresses, " ") != "203.0.113.7 203.0.113.8" {
		t.Fatalf("with two physical exit addresses the gate says %+v", tel)
	}

	// Not known any more, under an open gate: locked by that heartbeat,
	// before the next tick, and said at once.
	ended, before := held.ended, told
	unknown := tunnelBeat()
	unknown.PhysicalExitKnown = false
	g.heartbeat(unknown)
	if tel := g.telemetry(); held.held || held.ended != ended+1 || told != before+1 || tel.LockReason != sandbox.LockPhysicalExitUnknown {
		t.Fatalf("once they were unknown again: lease held %v, ended %d times, told %d times, telemetry %+v", held.held, held.ended-ended, told-before, tel)
	}
	g.answered(*now, 1, byTunnel)
	if held.held {
		t.Fatal("an answer opened the gate while they were unknown")
	}

	// Addresses that are said to be known but cannot be read are not
	// known: none of them is taken.
	for _, unreadable := range [][]string{{"203.0.113.7", "not an address"}, {"192.168.1.20"}, {"2001:db8::7"}, {""}} {
		g.heartbeat(tunnelBeat())
		g.answered(*now, 1, byTunnel)
		if !held.held {
			t.Fatal("the gate did not open again")
		}
		g.heartbeat(tunnelBeat(unreadable...))
		if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockPhysicalExitUnknown || tel.PhysicalExitKnown || len(tel.PhysicalExitAddresses) != 0 {
			t.Errorf("told the physical exit addresses %q: lease held %v, telemetry %+v", unreadable, held.held, tel)
		}
	}
}

// A sandbox whose Interface in use lists its exit addresses, or expects
// any, is not judged by this Mac's physical exit address: a heartbeat
// that says nothing of them leaves its gate as it is.
func TestGateWithoutTheExpectationIgnoresThisMacsPhysicalExitAddresses(t *testing.T) {
	g, held, now := tunnelGate() // en0 expects 203.0.113.7
	g.heartbeat(beat(true))
	g.answered(*now, 1, matched)
	if !held.held {
		t.Fatal("the gate did not open")
	}
	g.heartbeat(beat(true))
	g.lapse()
	if tel := g.telemetry(); !held.held || !tel.Open {
		t.Fatalf("a heartbeat that names no physical exit address locked a gate that needs none: %+v", tel)
	}
	// Nor is the address it expects refused for being this Mac's
	// physical exit address.
	physical := beat(true)
	physical.PhysicalExitAddresses, physical.PhysicalExitKnown = []string{"203.0.113.7"}, true
	g.heartbeat(physical)
	g.answered(*now, 1, matched)
	if tel := g.telemetry(); !held.held || !tel.Open {
		t.Fatalf("an expected exit address that is this Mac's physical exit address locked the gate: %+v", tel)
	}

	// The Interface in use becomes the tunnel, and the heartbeat that
	// says so knows nothing of this Mac's physical exit address:
	// locked for that.
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: tunnel, InterfaceChange: 2})
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockPhysicalExitUnknown {
		t.Fatalf("on the tunnel, unknown: lease held %v, telemetry %+v", held.held, tel)
	}
	// And back: en0 needs none.
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: "en0", InterfaceChange: 3})
	g.answered(*now, 3, matched)
	if !held.held {
		t.Fatal("back on en0 the gate did not open")
	}
}

// An answer with one of this Mac's physical exit addresses is an address
// that is not expected, believed for a lease like any other.
func TestGateLocksOnThisMacsPhysicalExitAddress(t *testing.T) {
	g, held, now := tunnelGate()
	g.heartbeat(tunnelBeat("203.0.113.7"))
	g.answered(*now, 1, byTunnel)
	if !held.held {
		t.Fatal("the tunnel's address did not open the gate")
	}

	sent := *now
	*now = now.Add(100 * time.Millisecond)
	g.answered(sent, 1, direct)
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "203.0.113.7" {
		t.Fatalf("answered with this Mac's physical exit address: lease held %v, telemetry %+v", held.held, tel)
	}
	// Another answer of the same round, and the next round's, open
	// nothing while it is believed.
	g.answered(sent, 1, byTunnel)
	*now = now.Add(2 * time.Second)
	g.heartbeat(tunnelBeat("203.0.113.7"))
	g.answered(*now, 1, byTunnel)
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "203.0.113.7" {
		t.Fatalf("while it was believed: lease held %v, telemetry %+v", held.held, tel)
	}
	*now = sent.Add(sandbox.LeaseDuration)
	g.heartbeat(tunnelBeat("203.0.113.7"))
	g.answered(*now, 1, byTunnel)
	if !held.held {
		t.Fatal("the gate stayed locked once the answer was no longer believed")
	}

	// An answer over IPv6 is one the prober does not take for expected
	// there: a mismatch as any.
	g.answered(*now, 1, probeAnswer{exit: "2001:db8::7"})
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "2001:db8::7" {
		t.Fatalf("answered over IPv6: lease held %v, telemetry %+v", held.held, tel)
	}
}

// The gate is open on an address, and the app then learns that it is
// this Mac's physical exit address: the tunnel had begun to lead there
// before the Host Watchdog read it. The heartbeat that says so locks
// the gate, for the address that opened it, and no answer still on its
// way opens it again.
func TestGateLocksWhenTheAddressItIsOpenOnBecomesPhysical(t *testing.T) {
	for _, second := range []bool{false, true} {
		g, held, now := tunnelGate()
		told := 0
		g.changed = func() { told++ }
		g.heartbeat(tunnelBeat("203.0.113.200"))

		// A round: the tunnel's address first, or this Mac's physical
		// exit address as its second answer, which renews nothing and
		// is kept all the same.
		sent := *now
		*now = now.Add(50 * time.Millisecond)
		if second {
			g.answered(sent, 1, byTunnel)
		}
		g.answered(sent, 1, direct)
		if tel := g.telemetry(); !held.held || !tel.Open {
			t.Fatalf("second answer %v: the gate is not open: %+v", second, tel)
		}
		// A later round is on its way.
		*now = now.Add(time.Second)
		inFlight := *now

		// The Host Watchdog has read this Mac's physical exit address.
		*now = now.Add(200 * time.Millisecond)
		ended, before := held.ended, told
		g.heartbeat(tunnelBeat("203.0.113.7"))
		tel := g.telemetry()
		if held.held || held.ended != ended+1 || told != before+1 || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "203.0.113.7" {
			t.Fatalf("second answer %v: once the address was this Mac's physical exit address: lease held %v, ended %d times, told %d times, telemetry %+v", second, held.held, held.ended-ended, told-before, tel)
		}
		// The answers on their way: this Mac's physical exit
		// address again, and the tunnel's from a moment when it
		// still led there.
		g.answered(inFlight, 1, direct)
		g.answered(inFlight, 1, byTunnel)
		if held.held {
			t.Fatalf("second answer %v: an answer on its way opened the gate again", second)
		}
		// It is believed for a lease from when the later of the probes so
		// answered was sent, and then the tunnel's address opens the gate.
		*now = inFlight.Add(sandbox.LeaseDuration - time.Millisecond)
		g.heartbeat(tunnelBeat("203.0.113.7"))
		g.answered(*now, 1, byTunnel)
		if held.held {
			t.Fatalf("second answer %v: the gate opened while the address was believed", second)
		}
		*now = inFlight.Add(sandbox.LeaseDuration)
		g.heartbeat(tunnelBeat("203.0.113.7"))
		g.answered(*now, 1, byTunnel)
		if !held.held {
			t.Fatalf("second answer %v: the gate stayed locked once it was no longer believed", second)
		}
	}

	// An answer older than a lease is not held against the gate, and
	// neither is one by the Interface that was in use before.
	g, held, now := tunnelGate()
	g.heartbeat(tunnelBeat())
	g.answered(*now, 1, direct)
	*now = now.Add(sandbox.LeaseDuration)
	g.heartbeat(tunnelBeat())
	g.answered(*now, 1, byTunnel)
	g.heartbeat(tunnelBeat("203.0.113.7"))
	if tel := g.telemetry(); !held.held || !tel.Open {
		t.Fatalf("an answer older than a lease locked the gate: %+v", tel)
	}
	g.heartbeat(tunnelBeat())
	g.answered(*now, 1, direct)
	change := sandbox.Heartbeat{HostConditionsMet: true, Interface: "utun10", InterfaceChange: 2, PhysicalExitAddresses: []string{"203.0.113.7"}, PhysicalExitKnown: true}
	g.butPhysical["utun10"] = true
	g.heartbeat(change)
	if tel := g.telemetry(); tel.LockReason != sandbox.LockProbeUnanswered || tel.ExitAddress != "" {
		t.Fatalf("an answer by the Interface before is held against the one in use: %+v", tel)
	}
}

// Of the reasons that forbid an open gate the weightiest is said, and
// this Mac's physical exit addresses are taken only once a change of the
// Interface in use is settled.
func TestTheOrderOfTheReasonsWithThisMacsPhysicalExitAddresses(t *testing.T) {
	g, held, now := tunnelGate()
	unknown := sandbox.Heartbeat{HostConditionsMet: true, Interface: tunnel, InterfaceChange: 1}

	// A mismatch and unknown: unknown is said.
	g.heartbeat(tunnelBeat("203.0.113.7"))
	g.answered(*now, 1, direct)
	if got := g.telemetry().LockReason; got != sandbox.LockExitAddressMismatch {
		t.Fatalf("answered with this Mac's physical exit address the gate says %q", got)
	}
	g.heartbeat(unknown)
	if got := g.telemetry().LockReason; got != sandbox.LockPhysicalExitUnknown {
		t.Errorf("with a mismatch believed and the physical exit addresses unknown the gate says %q", got)
	}
	// Known again, the mismatch is what stands, at that heartbeat.
	g.heartbeat(tunnelBeat("203.0.113.7"))
	if got := g.telemetry().LockReason; got != sandbox.LockExitAddressMismatch {
		t.Errorf("known again, with the mismatch still believed, the gate says %q", got)
	}
	// A Host Condition unmet outweighs unknown, no Interface outweighs
	// both, and heartbeats that stop outweigh all.
	unmet := unknown
	unmet.HostConditionsMet = false
	g.heartbeat(unmet)
	if got := g.telemetry().LockReason; got != sandbox.LockHostCondition {
		t.Errorf("with a Host Condition unmet and the physical exit addresses unknown the gate says %q", got)
	}
	g.heartbeat(sandbox.Heartbeat{InterfaceChange: 2})
	if got := g.telemetry().LockReason; got != sandbox.LockNoInterface {
		t.Errorf("with no Interface in use the gate says %q", got)
	}
	unknown.InterfaceChange = 3
	g.heartbeat(unknown)
	*now = now.Add(sandbox.LeaseDuration + time.Second)
	g.lapse()
	if got := g.telemetry().LockReason; got != sandbox.LockHeartbeatLost {
		t.Errorf("with the heartbeats stopped the gate says %q", got)
	}

	// A change of the Interface in use whose lease the kernel will not
	// end: the gate takes no note of the change, nor of the physical
	// exit addresses that came with it, which are those beside the new
	// one.
	g, held, now = tunnelGate()
	g.heartbeat(tunnelBeat("203.0.113.7"))
	g.answered(*now, 1, byTunnel)
	held.failEnd = errors.New("nft: busy")
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: "en0", InterfaceChange: 2, PhysicalExitAddresses: []string{"198.51.100.9"}, PhysicalExitKnown: true})
	if tel := g.telemetry(); tel.Open || tel.Interface != tunnel || strings.Join(tel.PhysicalExitAddresses, " ") != "203.0.113.7" {
		t.Fatalf("a change that waits for its lease to end: %+v", tel)
	}
	held.failEnd = nil
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: "en0", InterfaceChange: 2, PhysicalExitAddresses: []string{"198.51.100.9"}, PhysicalExitKnown: true})
	if tel := g.telemetry(); tel.Interface != "en0" || strings.Join(tel.PhysicalExitAddresses, " ") != "198.51.100.9" {
		t.Fatalf("once the lease was ended: %+v", tel)
	}
}

// physicalGate is a gate whose Interface in use expects this Mac's physical
// exit address, and no other: the sandbox exits where this Mac's physical
// Interfaces do.
func physicalGate() (*gate, *fakeLease, *time.Time) {
	g, held, now := testGate()
	g.onlyPhysical = map[string]bool{tunnel: true}
	return g, held, now
}

// Where the Interface in use expects this Mac's physical exit address, the
// gate opens on an answer with one of those and on no other, and is
// locked while the app does not say that it knows them.
func TestGateExpectsThisMacsPhysicalExitAddress(t *testing.T) {
	g, held, now := physicalGate()

	// Not known: locked for that, whatever a probe answers, and nothing
	// is held against the answer.
	g.heartbeat(sandbox.Heartbeat{HostConditionsMet: true, Interface: tunnel, InterfaceChange: 1})
	g.answered(*now, 1, direct)
	g.lapse()
	if tel := g.telemetry(); held.held || tel.Open || tel.LockReason != sandbox.LockPhysicalExitUnknown {
		t.Fatalf("with this Mac's physical exit addresses unknown: lease held %v, telemetry %+v", held.held, tel)
	}

	// Known: the answer with this Mac's physical exit
	// address opens the gate.
	g.heartbeat(tunnelBeat("203.0.113.7"))
	if held.held {
		t.Fatal("a heartbeat opened the gate")
	}
	g.answered(*now, 1, direct)
	if tel := g.telemetry(); !held.held || !tel.Open || tel.ExitAddress != "203.0.113.7" || strings.Join(tel.PhysicalExitAddresses, " ") != "203.0.113.7" {
		t.Fatalf("an answer with this Mac's physical exit address: lease held %v, telemetry %+v", held.held, tel)
	}

	// An answer with another address, as a tunnel's far end is: locked
	// at once, for an address that is not expected, which is shown.
	*now = now.Add(time.Second)
	g.answered(*now, 1, byTunnel)
	if tel := g.telemetry(); held.held || tel.Open || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "198.51.100.9" {
		t.Fatalf("an answer with another address: lease held %v, telemetry %+v", held.held, tel)
	}
	// And no answer with this Mac's physical exit address opens it
	// again while that one is believed.
	g.answered(*now, 1, direct)
	if held.held {
		t.Fatal("an answer opened the gate while another said the sandbox exits elsewhere")
	}
	*now = now.Add(sandbox.LeaseDuration)
	g.heartbeat(tunnelBeat("203.0.113.7"))
	g.answered(*now, 1, direct)
	if tel := g.telemetry(); !held.held || !tel.Open {
		t.Fatalf("once the other answer was no longer believed: lease held %v, telemetry %+v", held.held, tel)
	}

	// Known with none: this Mac has no way out of its own that answers.
	// Nothing the probe sees is this Mac's physical exit address.
	*now = now.Add(time.Second)
	g.heartbeat(tunnelBeat())
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "203.0.113.7" {
		t.Fatalf("with no physical exit address: lease held %v, telemetry %+v", held.held, tel)
	}
}

// The gate is open on an address that then stops being this Mac's
// physical exit address, as when the Mac moves to another network
// under a tunnel that stays: the heartbeat that says so locks it, from
// when the last probe so answered was sent, and an answer still on its
// way does not open it.
func TestGateLocksWhenTheAddressItIsOpenOnIsNoLongerPhysical(t *testing.T) {
	g, held, now := physicalGate()
	g.heartbeat(tunnelBeat("203.0.113.7"))
	sent := *now
	g.answered(sent, 1, direct)
	if !held.held {
		t.Fatal("the gate did not open")
	}
	*now = now.Add(2 * time.Second)
	ended := held.ended
	g.heartbeat(tunnelBeat("203.0.113.99"))
	if tel := g.telemetry(); held.held || held.ended != ended+1 || tel.LockReason != sandbox.LockExitAddressMismatch || tel.ExitAddress != "203.0.113.7" {
		t.Fatalf("once the address was not this Mac's physical exit address: lease held %v, ended %d times, telemetry %+v", held.held, held.ended-ended, tel)
	}
	// A second answer to the same Round, with the same address.
	g.answered(sent, 1, direct)
	if held.held {
		t.Fatal("an answer with an address that is no longer this Mac's physical exit address opened the gate")
	}
	// The sandbox now exits at what is this Mac's physical exit
	// address: open again, once the earlier answer is no longer
	// believed.
	*now = sent.Add(sandbox.LeaseDuration)
	g.heartbeat(tunnelBeat("203.0.113.99"))
	g.answered(*now, 1, probeAnswer{exit: "203.0.113.99", matches: true})
	if tel := g.telemetry(); !held.held || !tel.Open || tel.ExitAddress != "203.0.113.99" {
		t.Fatalf("at this Mac's new physical exit address: lease held %v, telemetry %+v", held.held, tel)
	}

	// Not known any more under an open gate: locked for that, and the
	// answer the gate was open on is not held against the sandbox.
	*now = now.Add(time.Second)
	unknown := tunnelBeat()
	unknown.PhysicalExitKnown = false
	g.heartbeat(unknown)
	if tel := g.telemetry(); held.held || tel.LockReason != sandbox.LockPhysicalExitUnknown {
		t.Fatalf("once they were unknown: lease held %v, telemetry %+v", held.held, tel)
	}
	g.heartbeat(tunnelBeat("203.0.113.99"))
	g.answered(*now, 1, probeAnswer{exit: "203.0.113.99", matches: true})
	if !held.held {
		t.Fatal("known again, an answer with this Mac's physical exit address did not open the gate")
	}
}

// standInConfig is a config whose sandbox may leave by utun4, where
// names are answered with stand-in addresses, and then by en0, where
// they are not.
func standInConfig() sandbox.GatekeeperConfig {
	cfg := gateConfig()
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{
		{Name: "utun4", AnyExitAddress: true, StandInAddresses: true},
		{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7"}},
	}
	return cfg
}

// A ruleset has the stand-in addresses' set and rules only for a config
// with an Allowed Interface where names are answered with them. The set
// is empty when the ruleset is loaded: nothing reaches the range until
// the gate has taken note of such an Interface in use. Each of the
// rules is a rule for public addresses again, with the set in the place
// of the public addresses: under the probe's mark or the lease, by the
// device, and nothing else.
func TestRulesetReachesStandInAddressesOnlyByTheirSet(t *testing.T) {
	plain, err := ruleset(gateConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "standin") {
		t.Error("a ruleset whose config has no Interface that answers with stand-in addresses names their set")
	}

	for _, hold := range []bool{false, true} {
		cfg := standInConfig()
		cfg.Hold = hold
		rules, err := ruleset(cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"set standin { type ipv4_addr; flags interval; }",
			`meta mark 0x1 oifname "tun0" ip daddr @standin tcp dport 443 accept`,
			`oifname "tun0" ip daddr @standin meta l4proto tcp meta mark @open counter name passed accept`,
			`iifname "tun0" ip saddr @standin ct state established accept`,
		}
		held := `oifname "tun0" ip daddr @standin tcp flags & (syn | ack) != syn ct state established meta mark @held counter name passed accept`
		if hold {
			want = append(want, held)
		} else if strings.Contains(rules, held) {
			t.Error("a ruleset with no Hold holds what goes to stand-in addresses")
		}
		for _, rule := range want {
			if !strings.Contains(rules, rule) {
				t.Errorf("with a Hold %v, the ruleset lacks %q", hold, rule)
			}
		}
		// The set is loaded empty, and names nothing but itself: no range
		// stands in the ruleset besides the ones that are refused.
		if strings.Count(rules, sandbox.StandInAddresses.String()) != 1 {
			t.Errorf("the stand-in addresses' range stands in the loaded ruleset %d times: once, among the ranges that are not public", strings.Count(rules, sandbox.StandInAddresses.String()))
		}
		// Every rule of the plain ruleset is there, in its order: the
		// stand-in addresses' rules are added, and replace none.
		plainCfg := cfg
		plainCfg.AllowedInterfaces = gateConfig().AllowedInterfaces
		without, _ := ruleset(plainCfg)
		at := 0
		for _, line := range strings.Split(without, "\n") {
			found := strings.Index(rules[at:], line+"\n")
			if found < 0 {
				t.Fatalf("with a Hold %v, the ruleset with stand-in addresses lacks, or has out of order, %q", hold, line)
			}
			at += found + len(line)
		}
		// In the output chain each accept that names the set is under the
		// probe's mark or the lease, or the Hold, and by the device.
		accepts := 0
		for _, line := range outputChain(t, rules) {
			if !strings.Contains(line, "@standin") {
				continue
			}
			accepts++
			byProbe := strings.HasPrefix(line, `meta mark 0x1 oifname "tun0" ip daddr @standin tcp dport 443 `)
			byLease := strings.HasPrefix(line, `oifname "tun0" ip daddr @standin `) && (strings.Contains(line, "meta mark @open") || strings.Contains(line, "meta mark @held"))
			if !strings.HasSuffix(line, "accept") || !byProbe && !byLease {
				t.Errorf("the output chain passes to stand-in addresses what neither the lease nor the probe's mark vouches for: %s", line)
			}
			if strings.Contains(line, "@held") && (!strings.Contains(line, "!= syn") || !strings.Contains(line, "ct state established")) {
				t.Errorf("under a Hold a connection is made to a stand-in address: %s", line)
			}
		}
		if wantAccepts := map[bool]int{false: 2, true: 3}[hold]; accepts != wantAccepts {
			t.Errorf("with a Hold %v, %d rules of the output chain name the stand-in addresses' set, want %d", hold, accepts, wantAccepts)
		}
	}
}

// The script that says whether the stand-in addresses are reached puts
// their range in the set, or leaves the set empty, and the identity of
// a ruleset is the same either way: the set's element comes and goes as
// the lease does.
func TestTheStandInAddressesSetComesAndGoesWithTheInterfaceInUse(t *testing.T) {
	if got, want := standInScript(true), "flush set inet zeolite standin\nadd element inet zeolite standin { 198.18.0.0/15 }\n"; got != want {
		t.Errorf("the script that has the stand-in addresses reached is %q, want %q", got, want)
	}
	if got, want := standInScript(false), "flush set inet zeolite standin\n"; got != want {
		t.Errorf("the script that has them not reached is %q, want %q", got, want)
	}
	empty := "table inet zeolite {\n\tset open {\n\t\ttypeof meta mark\n\t\tflags timeout\n\t}\n\n\tset nonpublic4 {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\telements = { 10.0.0.0/8, 198.18.0.0/15 }\n\t}\n\n\tset standin {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t}\n}\n"
	reached := strings.Replace(empty, "set standin {\n\t\ttype ipv4_addr\n\t\tflags interval\n", "set standin {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\telements = { 198.18.0.0/15 }\n", 1)
	if reached == empty {
		t.Fatal("the test's listings do not differ")
	}
	if rulesetIdentity(reached) != rulesetIdentity(empty) {
		t.Errorf("a ruleset whose stand-in addresses are reached and one whose are not have different identities:\n%s", rulesetIdentity(reached))
	}
	if !strings.Contains(rulesetIdentity(reached), "10.0.0.0/8, 198.18.0.0/15") {
		t.Error("the identity lost the set of the ranges that are not public")
	}
	if without := strings.Replace(empty, "\n\tset standin {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t}\n", "", 1); without == empty || rulesetIdentity(without) == rulesetIdentity(empty) {
		t.Error("a ruleset with the stand-in addresses' set has the identity of one without")
	}
}

// The gate has the stand-in addresses reached by an Interface in use
// where names are answered with them, and by no other: it says so to
// the kernel in the order that ends the lease kept under the Interface
// before, and at no other time. Where the kernel refuses that order,
// the gate takes no note of the Interface and says it again.
func TestGateReachesStandInAddressesOnlyByTheInterfaceThatAnswersWithThem(t *testing.T) {
	g, kernel, now := testGate()
	g.standIns = map[string]bool{"utun4": true}
	beatOf := func(iface string, change uint64) sandbox.Heartbeat {
		return sandbox.Heartbeat{HostConditionsMet: true, Interface: iface, InterfaceChange: change}
	}

	g.heartbeat(beatOf("utun4", 1))
	if !kernel.standIns || kernel.turned != 1 {
		t.Fatalf("by an Interface that answers with stand-in addresses: reached %v, said %d times", kernel.standIns, kernel.turned)
	}
	// A lease that is renewed, a lock and a heartbeat that changes
	// nothing say nothing of it.
	g.answered(*now, 1, matched)
	g.heartbeat(beatOf("utun4", 1))
	g.heartbeat(sandbox.Heartbeat{Interface: "utun4", InterfaceChange: 1})
	if !kernel.standIns || kernel.turned != 1 {
		t.Fatalf("with the Interface in use as it was: reached %v, said %d times", kernel.standIns, kernel.turned)
	}

	// The next Interface answers with none.
	g.heartbeat(beatOf("en0", 2))
	if kernel.standIns || kernel.turned != 2 || kernel.held {
		t.Fatalf("by an Interface that answers with none: reached %v, said %d times, lease held %v", kernel.standIns, kernel.turned, kernel.held)
	}
	// Nor does no Interface at all.
	g.heartbeat(beatOf("utun4", 3))
	g.heartbeat(beatOf("", 4))
	if kernel.standIns || kernel.turned != 4 {
		t.Fatalf("with no Interface in use: reached %v, said %d times", kernel.standIns, kernel.turned)
	}

	// The kernel refuses: the gate is where it was, and says it again.
	kernel.failEnd = errors.New("nft: busy")
	g.heartbeat(beatOf("utun4", 5))
	if tel := g.telemetry(); kernel.standIns || tel.Interface != "" || tel.InterfaceChange != 4 {
		t.Fatalf("an Interface whose order the kernel refused was taken note of: %+v, stand-in addresses reached %v", tel, kernel.standIns)
	}
	kernel.failEnd = nil
	g.heartbeat(beatOf("utun4", 5))
	if tel := g.telemetry(); !kernel.standIns || kernel.turned != 5 || tel.Interface != "utun4" {
		t.Fatalf("at the next heartbeat: reached %v, said %d times, %+v", kernel.standIns, kernel.turned, tel)
	}
}

// A probe that is sent by an Interface where names are answered with
// stand-in addresses says that it reaches them, and so does the
// timezone's; one sent by another Interface does not.
func TestAProbeReachesStandInAddressesOnlyByTheInterfaceThatAnswersWithThem(t *testing.T) {
	a := answers{probeURL: "203.0.113.7\n", timezoneURL: "Europe/Berlin\n", reachedStandIns: ""}
	cfg := standInConfig()
	p, err := newProber(cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, iface := range []string{"utun4", "en0", "utun9"} {
		if _, err := p.ask(ctx, probeURL, iface); err != nil {
			t.Fatal(err)
		}
		if _, err := p.timezone(ctx, timezoneURL, iface); err != nil {
			t.Fatal(err)
		}
	}
	want := probeURL + " true\n" + timezoneURL + " true\n" + probeURL + " false\n" + timezoneURL + " false\n" + probeURL + " false\n" + timezoneURL + " false\n"
	if a[reachedStandIns] != want {
		t.Errorf("the probes by utun4, en0 and an Interface the config does not name said:\n%swant:\n%s", a[reachedStandIns], want)
	}
}
