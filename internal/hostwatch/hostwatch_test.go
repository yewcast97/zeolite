package hostwatch

import (
	"net"
	"testing"

	"zeolite/internal/sandbox"
)

func TestParseSSID(t *testing.T) {
	summary := `<dictionary> {
  BSSID : aa:bb:cc:dd:ee:ff
  InterfaceType : WiFi
  LinkStatusActive : TRUE
  SSID : Office Network 5G
  Security : WPA2_PSK
}`
	if ssid, ok := parseSSID(summary); !ok || ssid != "Office Network 5G" {
		t.Errorf("parsed %q, %v", ssid, ok)
	}
	// BSSID is not the SSID.
	if _, ok := parseSSID("  BSSID : aa:bb:cc:dd:ee:ff\n  InterfaceType : Ethernet\n"); ok {
		t.Error("a summary without an SSID gave one")
	}
	if _, ok := parseSSID("  SSID : <redacted>\n"); ok {
		t.Error("a withheld name counts as a name")
	}
}

func TestMet(t *testing.T) {
	reads := 0
	w := &Watchdog{readSSIDs: func() []string { reads++; return []string{"Office"} }}
	office := sandbox.HostCondition{Kind: sandbox.WiFiSSID, Value: "Office"}
	cafe := sandbox.HostCondition{Kind: sandbox.WiFiSSID, Value: "Cafe"}

	if !w.Met(nil) {
		t.Error("no conditions are unmet")
	}
	if !w.Met([]sandbox.HostCondition{office}) {
		t.Error("a condition that holds is unmet")
	}
	// A condition that does not hold is unmet, and so is one of a kind
	// the Watchdog does not know.
	for _, unmet := range [][]sandbox.HostCondition{{cafe}, {office, cafe}, {{Kind: "bogus", Value: "x"}}} {
		if w.Met(unmet) {
			t.Errorf("%v is met", unmet)
		}
	}
	if reads != 1 {
		t.Errorf("the Mac was read %d times within a second", reads)
	}

	// A name that cannot be read is unmet.
	w = &Watchdog{readSSIDs: func() []string { return nil }}
	if w.Met([]sandbox.HostCondition{office}) {
		t.Error("an unreadable network name is met")
	}
}

// The interfaces offered are ones a sandbox could leave by now: each has
// a name an Allowed Interface takes and an address, and loopback, which
// leads nowhere, is not among them.
func TestThisMacsInterfaces(t *testing.T) {
	seen := map[string]bool{}
	for _, ifc := range Interfaces() {
		if !sandbox.ValidInterfaceName(ifc.Name) || ifc.Name == "lo0" {
			t.Errorf("%q is offered as an Interface", ifc.Name)
		}
		if seen[ifc.Name] {
			t.Errorf("%s is offered twice", ifc.Name)
		}
		seen[ifc.Name] = true
		if ifi, err := net.InterfaceByName(ifc.Name); err != nil || ipv4Of(*ifi) == "" {
			t.Errorf("%s is offered with no IPv4 address (%v)", ifc.Name, err)
		}
	}
}

// What the system calls a port is read from its own listing, by the
// interface's name; an interface that is no hardware port has no title.
func TestParsePorts(t *testing.T) {
	const listing = `
Hardware Port: Ethernet Adapter (en3)
Device: en3
Ethernet Address: 02:00:00:00:00:01

Hardware Port: Wi-Fi
Device: en0
Ethernet Address: 02:00:00:00:00:02

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: 02:00:00:00:00:03

VLAN Configurations
===================
`
	got := parsePorts(listing)
	want := map[string]string{"en3": "Ethernet Adapter (en3)", "en0": "Wi-Fi", "bridge0": "Thunderbolt Bridge"}
	if len(got) != len(want) {
		t.Fatalf("the ports read are %v", got)
	}
	for name, title := range want {
		if got[name] != title {
			t.Errorf("%s is titled %q, want %q", name, got[name], title)
		}
	}
	if len(parsePorts("networksetup: something went wrong\n")) != 0 {
		t.Error("titles were read from what is no listing")
	}
}
