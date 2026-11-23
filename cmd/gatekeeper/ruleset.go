package main

import (
	"fmt"
	"strings"

	"zeolite/internal/sandbox"
)

// Socket marks, which the ruleset and the gatekeeper's sockets agree on.
// Setting one takes NET_ADMIN, which only the gatekeeper has, so a mark
// says "this socket is the gatekeeper's", whatever user a process in the
// workstation runs as.
const (
	// markProbe is on the probe's sockets: they reach the resolver and
	// port 443 of public addresses, whatever the lease says.
	markProbe = 0x1
	// markNone is what every other socket in the namespace carries. It is
	// the lease's element: unmarked packets pass while the set holds it.
	markNone = 0x0
)

// ruleset returns the nftables script for a config. It is loaded once,
// in one transaction, while the namespace has no device but loopback,
// and never edited: the only thing that changes afterwards is the
// element of the set "open", which is the lease, and of the set "held",
// which is the Hold, where the config asks for one.
//
// The namespace gets one device besides loopback, the TUN device, and
// every accept that is not loopback's names it: a packet leaves by that
// device or not at all, whatever other device might one day be there.
//
// Each rule here was checked on the Colima VM's kernel with counters and
// a packet capture on the sandbox's device. Things that look removable
// and are not:
//
//   - The probe's rules come first, ahead of loopback: a probe that was
//     pointed at an address of the sandbox itself is rejected, so nothing
//     in the sandbox can answer one.
//   - What comes from one of the Probe Source Ports without the probe's
//     mark is rejected, whatever the lease says: the Carrier takes a
//     connection from one for a probe's, and sends it on while the
//     sandbox's own wait. Loopback comes first: a server in the sandbox
//     may listen on any port.
//   - Output has no `ct state established` accept that stands by
//     itself, and there is no NAT chain: every packet that leaves is a
//     lookup of the lease, or of the Hold. With an accept of established
//     flows, a lease that expired would not cut the flows that are
//     already open.
//   - The Hold's rule is there only for a config that asks for a Hold,
//     and its set with it: no other gate can hold anything. It passes
//     what belongs to a TCP connection that was answered, for as long as
//     the set holds the mark, and never a SYN: a connection is made
//     under the lease alone, also one that takes the ports of one that
//     was. A name query is asked under the lease alone: any exit address
//     answers one.
//   - The rules of the stand-in addresses are there only for a config
//     with an Allowed Interface where names are answered with them, and
//     their set with them. The set holds the range while such an
//     Interface is the one in use, and nothing otherwise: it is given
//     its element, or loses it, in the transaction that ends the lease
//     kept under the Interface before. Each of those rules is one of
//     the rules for public addresses again, for that set: the probe's,
//     the lease's, the Hold's, and what is received.
//   - Everything is rejected, not dropped, so that a locked gate fails
//     at once instead of timing out.
func ruleset(cfg sandbox.GatekeeperConfig) (string, error) {
	resolver, err := sandbox.ParsePublicIPv4(cfg.Resolver)
	if err != nil {
		return "", fmt.Errorf("the resolver %q: %w", cfg.Resolver, err)
	}
	var nonPublic []string
	for _, p := range sandbox.NonPublicPrefixes {
		if p.Addr().Is4() {
			nonPublic = append(nonPublic, p.String())
		}
	}
	standIns := answersWithStandIns(cfg)
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("table inet zeolite {")
	w("  counter passed { }")
	w("  counter rejected { }")
	w("  set open { typeof meta mark; flags timeout; }")
	if cfg.Hold {
		w("  set held { typeof meta mark; flags timeout; }")
	}
	w("  set nonpublic4 { type ipv4_addr; flags interval; elements = { %s } }", strings.Join(nonPublic, ", "))
	if standIns {
		w("  set standin { type ipv4_addr; flags interval; }")
	}
	w("")
	w("  chain gate_output {")
	w("    type filter hook output priority 0; policy drop;")
	w("    meta mark %#x oifname %q ip daddr %s meta l4proto { tcp, udp } th dport 53 accept", markProbe, sandbox.TUNDevice, resolver)
	w("    meta mark %#x oifname %q ip daddr != @nonpublic4 tcp dport %s accept", markProbe, sandbox.TUNDevice, sandbox.ProbePort)
	if standIns {
		w("    meta mark %#x oifname %q ip daddr @standin tcp dport %s accept", markProbe, sandbox.TUNDevice, sandbox.ProbePort)
	}
	w("    meta mark %#x counter name rejected reject", markProbe)
	w("    oifname \"lo\" fib daddr type local accept")
	w("    meta l4proto { tcp, udp } th sport %d-%d counter name rejected reject", sandbox.ProbeSourcePortFirst, sandbox.ProbeSourcePortLast)
	w("    oifname %q ip daddr %s udp dport 53 meta mark @open counter name passed accept", sandbox.TUNDevice, resolver)
	w("    oifname %q ip daddr != @nonpublic4 meta l4proto tcp meta mark @open counter name passed accept", sandbox.TUNDevice)
	if standIns {
		w("    oifname %q ip daddr @standin meta l4proto tcp meta mark @open counter name passed accept", sandbox.TUNDevice)
	}
	if cfg.Hold {
		w("    oifname %q ip daddr != @nonpublic4 tcp flags & (syn | ack) != syn ct state established meta mark @held counter name passed accept", sandbox.TUNDevice)
		if standIns {
			w("    oifname %q ip daddr @standin tcp flags & (syn | ack) != syn ct state established meta mark @held counter name passed accept", sandbox.TUNDevice)
		}
	}
	w("    meta l4proto tcp counter name rejected reject with tcp reset")
	w("    counter name rejected reject")
	w("  }")
	w("")
	w("  chain gate_input {")
	w("    type filter hook input priority 0; policy drop;")
	w("    iifname \"lo\" accept")
	w("    iifname %q ip saddr != @nonpublic4 ct state established accept", sandbox.TUNDevice)
	if standIns {
		w("    iifname %q ip saddr @standin ct state established accept", sandbox.TUNDevice)
	}
	w("    iifname %q ct state related icmp type destination-unreachable accept", sandbox.TUNDevice)
	w("  }")
	w("")
	// Unprivileged programs can bind a socket to lo and aim it at a
	// public address; without this chain the packet could be forwarded.
	w("  chain gate_forward { type filter hook forward priority 0; policy drop; }")
	w("}")
	return b.String(), nil
}

// leaseScript replaces the lease, the Hold, or both in one transaction:
// the set "open" holds the mark of the sandbox's own sockets for
// leaseMillis, and the set "held" for holdMillis. One that is zero is
// left as it is. Flushing and adding in one transaction is what renews
// an entry, since adding an element that is already there leaves its old
// expiry in place.
func leaseScript(leaseMillis, holdMillis int64) string {
	var b strings.Builder
	for _, entry := range []struct {
		set    string
		millis int64
	}{{"open", leaseMillis}, {"held", holdMillis}} {
		if entry.millis > 0 {
			fmt.Fprintf(&b, "flush set inet zeolite %s\nadd element inet zeolite %s { %#x timeout %dms }\n", entry.set, entry.set, markNone, entry.millis)
		}
	}
	return b.String()
}

// endScript ends the lease, and the Hold with it where the ruleset has
// one.
func endScript(hold bool) string {
	if hold {
		return "flush set inet zeolite open\nflush set inet zeolite held\n"
	}
	return "flush set inet zeolite open\n"
}

// answersWithStandIns is whether a config has an Allowed Interface
// where names are answered with stand-in addresses: its ruleset then
// has their set and their rules, and has neither otherwise.
func answersWithStandIns(cfg sandbox.GatekeeperConfig) bool {
	for _, ifc := range cfg.AllowedInterfaces {
		if ifc.StandInAddresses {
			return true
		}
	}
	return false
}

// standInScript makes the set of the stand-in addresses hold their
// range, or nothing: what the rules that name the set let pass by the
// Interface in use that follows. It is one transaction with the script
// that ends the lease, which stands before it.
func standInScript(reached bool) string {
	script := "flush set inet zeolite standin\n"
	if reached {
		script += "add element inet zeolite standin { " + sandbox.StandInAddresses.String() + " }\n"
	}
	return script
}

// checkLocalPortRange reads the ports a namespace hands out by itself,
// as the kernel prints them, and says whether one of the Probe Source
// Ports is among them.
func checkLocalPortRange(printed string) error {
	var first, last int
	if _, err := fmt.Sscan(printed, &first, &last); err != nil {
		return fmt.Errorf("the namespace's local port range %q could not be read: %w", strings.TrimSpace(printed), err)
	}
	if first <= sandbox.ProbeSourcePortLast && last >= sandbox.ProbeSourcePortFirst {
		return fmt.Errorf("the namespace hands out ports %d to %d by itself, among them the probe's %d to %d", first, last, sandbox.ProbeSourcePortFirst, sandbox.ProbeSourcePortLast)
	}
	return nil
}
