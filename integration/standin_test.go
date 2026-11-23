//go:build integration

package integration

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
	"zeolite/internal/sandboximage"
)

// A sandbox connects to a stand-in address only by an Allowed Interface
// where names are said to be answered with them. Three sandboxes leave
// by this Mac's physical Interface: one whose Setup says so, one that
// says so and keeps a Hold, and one that does not say so.
//
// The Interface answers no name so, and leads to no stand-in address:
// what is tested here is what the ruleset lets on the device, on the
// VM's kernel, and that the Carrier takes it. That a stand-in address
// answers, by a tunnel that hands them out, is not tested here.
func TestStandInAddressesAreReachedOnlyWhereNamesAreAnsweredWithThem(t *testing.T) {
	standIn := netip.MustParseAddr("198.18.0.13")
	said := openConfig(t, "standin-said")
	said.AllowedInterfaces[0].StandInAddresses = true
	held := openConfig(t, "standin-held")
	held.AllowedInterfaces[0].StandInAddresses = true
	held.Gate.Hold = true
	unsaid := openConfig(t, "standin-unsaid")
	for _, cfg := range []sandbox.Config{said, held, unsaid} {
		start(t, cfg, nil, nil)
	}
	for _, cfg := range []sandbox.Config{said, held, unsaid} {
		waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	}

	// The set in the kernel holds the range where the Interface in use
	// answers with stand-in addresses, and is not there where none does;
	// and the ruleset is the one that was loaded, though the set was
	// given its element since.
	set := func(id string) (string, int) {
		return docker("exec", orchestrator.GatekeeperContainer(id), "nft", "list", "set", "inet", "zeolite", "standin")
	}
	for _, cfg := range []sandbox.Config{said, held} {
		if out, code := set(cfg.ID); code != 0 || !strings.Contains(out, sandbox.StandInAddresses.String()) {
			t.Errorf("%s: the set of the stand-in addresses does not hold their range: %s", cfg.ID, trimmed(out))
		}
		if out, code := docker("exec", orchestrator.GatekeeperContainer(cfg.ID), sandboximage.GatekeeperPath, "verify", "n1"); code != 0 || !strings.Contains(out, `"ok":true`) {
			t.Errorf("%s: with the range in its set the ruleset is not found as it was loaded: %s", cfg.ID, trimmed(out))
		}
	}
	if out, code := set(unsaid.ID); code == 0 {
		t.Errorf("a ruleset whose sandbox answers no name with stand-in addresses has their set: %s", trimmed(out))
	}

	// toStandIn are the packets a sandbox put on its device for the
	// stand-in address.
	toStandIn := func(c *tunCapture) (packets []packet) {
		for _, p := range c.packets(t) {
			if p.dst == standIn {
				packets = append(packets, p)
			}
		}
		return packets
	}

	// Where it is not said, the ruleset refuses at once, and nothing for
	// the address is on the device.
	capture := captureTUN(t, unsaid.ID)
	if got := attempt(t, unsaid.ID, standIn.String(), 80); got != refused {
		t.Errorf("a stand-in address, where names are not answered with them, was %s, want %s", got, refused)
	}
	if got := toStandIn(capture); len(got) != 0 {
		t.Errorf("a sandbox whose way out answers no name with stand-in addresses put %d packets on its device for one: %s", len(got), got[0].line)
	}

	// Where it is said, the connection is on the device, and the Carrier
	// takes it: it is not refused at once. Nothing else that is not
	// public is on the device, and the public internet answers as before.
	for _, cfg := range []sandbox.Config{said, held} {
		capture := captureTUN(t, cfg.ID)
		if got := attempt(t, cfg.ID, standIn.String(), 80); got != timedOut {
			t.Errorf("%s: a stand-in address, by a way out that answers none, was %s, want %s: the Carrier makes its socket, and nothing answers", cfg.ID, got, timedOut)
		}
		sent := toStandIn(capture)
		if len(sent) == 0 {
			t.Errorf("%s: nothing for the stand-in address is on the device: the ruleset did not let it pass", cfg.ID)
		}
		for _, p := range sent {
			if !p.tcp() || p.dstPort != 80 || sandbox.ProbeSourcePort(p.srcPort) {
				t.Errorf("%s: on the device for the stand-in address: %s", cfg.ID, p.line)
			}
		}
		for what, host := range map[string]string{
			"a private address":                   "10.0.0.1",
			"the Mac, by the VM's address for it": hostGateway(t).String(),
			"the metadata address":                "169.254.169.254",
			"carrier-grade NAT":                   "100.64.0.1",
			"the sandbox's own loopback":          "127.0.0.1",
		} {
			if got := attempt(t, cfg.ID, host, 80); got != refused {
				t.Errorf("%s: %s (%s) was %s, want %s", cfg.ID, what, host, got, refused)
			}
		}
		for _, p := range capture.packets(t) {
			if !sandbox.PublicAddress(p.dst) && p.dst != standIn {
				t.Errorf("%s: on the device for an address that is neither public nor a stand-in address: %s", cfg.ID, p.line)
			}
		}
		if got := attempt(t, cfg.ID, "one.one.one.one", 80); got != answered {
			t.Errorf("%s: a public name was %s, want %s", cfg.ID, got, answered)
		}
	}
}
