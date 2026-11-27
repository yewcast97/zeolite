package sandbox

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// gated returns a valid config.
func gated() Config {
	c := NewConfig("s1", "Research")
	c.AllowedInterfaces = []AllowedInterface{{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7"}}}
	c.Gate.ProbeURLs = []string{"https://probe.example.com/ip"}
	return c
}

func fields(errs []FieldError) []Field {
	var fs []Field
	for _, e := range errs {
		fs = append(fs, e.Field)
	}
	return fs
}

func TestValidConfigs(t *testing.T) {
	conditions := gated()
	conditions.HostConditions = []HostCondition{{WiFiSSID, "Office"}}
	conditions.SharedFolder = "research-2"

	dual := gated()
	dual.AllowedInterfaces = []AllowedInterface{
		{Name: "utun4", ExpectedExitAddresses: []string{"203.0.113.7", "198.51.100.9"}},
		{Name: "en0", AnyExitAddress: true},
		{Name: "utun7", AnyExitAddressButPhysical: true},
		{Name: "en7", PhysicalExitAddress: true},
	}
	dual.Gate.ProbeURLs = []string{"https://probe.example.com:443/ip", "https://192.0.2.1/"}
	dual.Gate.Resolver = "9.9.9.9"
	dual.Gate.ProbePin = "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
	dual.Clock = ClockPolicy{Timezone: TimezoneAuto, TimezoneProbeURL: "https://probe.example.com/timezone"}

	// An ACP Bridge is given with a Shared Folder.
	bridged := gated()
	bridged.SharedFolder, bridged.ACPBridge = "research", true

	for name, c := range map[string]Config{"gated": gated(), "conditions": conditions, "dual": dual, "bridged": bridged} {
		if errs := c.Validate(); errs != nil {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

func TestInvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
		want   Field
	}{
		{"empty name", func(c *Config) { c.Name = "  " }, FieldName},
		{"upper-case ID", func(c *Config) { c.ID = "S1" }, FieldID},
		{"no Interface", func(c *Config) { c.AllowedInterfaces = nil }, FieldAllowedInterfaces},
		{"an Interface with a space in its name", func(c *Config) { c.AllowedInterfaces[0].Name = "en 0" }, FieldAllowedInterfaces},
		{"an Interface with no name", func(c *Config) { c.AllowedInterfaces[0].Name = "" }, FieldAllowedInterfaces},
		{"an Interface with a name too long", func(c *Config) { c.AllowedInterfaces[0].Name = "utun1234567890123" }, FieldAllowedInterfaces},
		{"an Interface listed twice", func(c *Config) {
			c.AllowedInterfaces = append(c.AllowedInterfaces, AllowedInterface{Name: "en0", AnyExitAddress: true})
		}, FieldAllowedInterfaces},
		{"an Interface with neither an exit address nor any", func(c *Config) { c.AllowedInterfaces[0].ExpectedExitAddresses = nil }, FieldAllowedInterfaces},
		{"an Interface with both an exit address and any", func(c *Config) { c.AllowedInterfaces[0].AnyExitAddress = true }, FieldAllowedInterfaces},
		{"an Interface with both an exit address and any but physical", func(c *Config) { c.AllowedInterfaces[0].AnyExitAddressButPhysical = true }, FieldAllowedInterfaces},
		{"an Interface with any and any but physical", func(c *Config) {
			c.AllowedInterfaces[0] = AllowedInterface{Name: "en0", AnyExitAddress: true, AnyExitAddressButPhysical: true}
		}, FieldAllowedInterfaces},
		{"an Interface with both an exit address and this Mac's physical exit address", func(c *Config) { c.AllowedInterfaces[0].PhysicalExitAddress = true }, FieldAllowedInterfaces},
		{"an Interface with this Mac's physical exit address and any but physical", func(c *Config) {
			c.AllowedInterfaces[0] = AllowedInterface{Name: "en0", PhysicalExitAddress: true, AnyExitAddressButPhysical: true}
		}, FieldAllowedInterfaces},
		{"an Interface with this Mac's physical exit address and any", func(c *Config) {
			c.AllowedInterfaces[0] = AllowedInterface{Name: "en0", PhysicalExitAddress: true, AnyExitAddress: true}
		}, FieldAllowedInterfaces},
		{"a second Interface with neither", func(c *Config) {
			c.AllowedInterfaces = append(c.AllowedInterfaces, AllowedInterface{Name: "utun4"})
		}, FieldAllowedInterfaces},
		{"exit address with a zone", func(c *Config) { c.AllowedInterfaces[0].ExpectedExitAddresses = []string{"fe80::1%en0"} }, FieldAllowedInterfaces},
		{"loopback exit address", func(c *Config) { c.AllowedInterfaces[0].ExpectedExitAddresses = []string{"127.0.0.1"} }, FieldAllowedInterfaces},
		{"private exit address", func(c *Config) { c.AllowedInterfaces[0].ExpectedExitAddresses = []string{"192.168.1.7"} }, FieldAllowedInterfaces},
		{"a stand-in exit address", func(c *Config) { c.AllowedInterfaces[0].ExpectedExitAddresses = []string{"198.18.0.7"} }, FieldAllowedInterfaces},
		{"IPv6 exit address", func(c *Config) { c.AllowedInterfaces[0].ExpectedExitAddresses = []string{"2001:db8::7"} }, FieldAllowedInterfaces},
		{"exit address that is a name", func(c *Config) { c.AllowedInterfaces[0].ExpectedExitAddresses = []string{"example.com"} }, FieldAllowedInterfaces},
		{"no probe", func(c *Config) { c.Gate.ProbeURLs = nil }, FieldProbeURLs},
		{"plain HTTP probe", func(c *Config) { c.Gate.ProbeURLs = []string{"http://probe.example.com/ip"} }, FieldProbeURLs},
		{"probe with credentials", func(c *Config) { c.Gate.ProbeURLs = []string{"https://user:pw@probe.example.com/ip"} }, FieldProbeURLs},
		{"probe on another port", func(c *Config) { c.Gate.ProbeURLs = []string{"https://probe.example.com:8443/ip"} }, FieldProbeURLs},
		{"probe at a private address", func(c *Config) { c.Gate.ProbeURLs = []string{"https://10.0.0.1/ip"} }, FieldProbeURLs},
		{"probe at loopback", func(c *Config) { c.Gate.ProbeURLs = []string{"https://127.0.0.1/ip"} }, FieldProbeURLs},
		{"probe at an IPv6 address", func(c *Config) { c.Gate.ProbeURLs = []string{"https://[2606:4700::1111]/ip"} }, FieldProbeURLs},
		{"probe at localhost", func(c *Config) { c.Gate.ProbeURLs = []string{"https://localhost/ip"} }, FieldProbeURLs},
		{"probe at a name under localhost", func(c *Config) { c.Gate.ProbeURLs = []string{"https://echo.localhost./ip"} }, FieldProbeURLs},
		{"probe at a name without a dot", func(c *Config) { c.Gate.ProbeURLs = []string{"https://nas/ip"} }, FieldProbeURLs},
		{"no resolver", func(c *Config) { c.Gate.Resolver = "" }, FieldResolver},
		{"resolver that is a name", func(c *Config) { c.Gate.Resolver = "dns.example.com" }, FieldResolver},
		{"private resolver", func(c *Config) { c.Gate.Resolver = "192.168.1.1" }, FieldResolver},
		{"IPv6 resolver", func(c *Config) { c.Gate.Resolver = "2606:4700:4700::1111" }, FieldResolver},
		{"resolver in the benchmarking range", func(c *Config) { c.Gate.Resolver = "198.18.0.2" }, FieldResolver},
		{"short pin", func(c *Config) { c.Gate.ProbePin = "AAAA" }, FieldProbePin},
		{"long SSID", func(c *Config) { c.HostConditions = []HostCondition{{WiFiSSID, strings.Repeat("x", 33)}} }, FieldHostConditions},
		{"a Host Condition of an unknown kind", func(c *Config) {
			c.HostConditions = []HostCondition{{"bluetooth_device", "Keyboard"}}
		}, FieldHostConditions},
		{"no timezone", func(c *Config) { c.Clock.Timezone = "" }, FieldTimezone},
		{"this Mac's timezone, whatever it is", func(c *Config) { c.Clock.Timezone = "Local" }, FieldTimezone},
		{"unknown timezone", func(c *Config) { c.Clock.Timezone = "Mars/Olympus" }, FieldTimezone},
		{"auto timezone without a probe", func(c *Config) { c.Clock.Timezone = TimezoneAuto }, FieldTimezoneProbeURL},
		{"auto timezone with a private probe", func(c *Config) {
			c.Clock.Timezone, c.Clock.TimezoneProbeURL = TimezoneAuto, "https://192.168.5.2/timezone"
		}, FieldTimezoneProbeURL},
		{"too little shm", func(c *Config) { c.Resources.ShmMB = 1024 }, FieldShm},
		{"memory not above shm", func(c *Config) { c.Resources.MemoryMB = c.Resources.ShmMB }, FieldMemory},
		{"no CPUs", func(c *Config) { c.Resources.CPUs = 0 }, FieldCPUs},
		{"pack twice", func(c *Config) { c.Packs = []string{"web-test-browser", "web-test-browser"} }, FieldPacks},
		{"a Shared Folder named as a path", func(c *Config) { c.SharedFolder = "../research" }, FieldSharedFolder},
		{"a Shared Folder with a capital", func(c *Config) { c.SharedFolder = "Research" }, FieldSharedFolder},
		{"a Shared Folder named as the root's marker", func(c *Config) { c.SharedFolder = ".zeolite-shared-folder-root" }, FieldSharedFolder},
		{"a Shared Folder with a long name", func(c *Config) { c.SharedFolder = strings.Repeat("x", 33) }, FieldSharedFolder},
		{"an ACP Bridge with no Shared Folder", func(c *Config) { c.ACPBridge = true }, FieldACPBridge},
	}
	for _, tt := range tests {
		c := gated()
		tt.change(&c)
		got := fields(c.Validate())
		if len(got) != 1 || got[0] != tt.want {
			t.Errorf("%s: errors on %v, want only %v", tt.name, got, tt.want)
		}
	}
}

func TestParseExitAddressUnmaps(t *testing.T) {
	a, err := ParseExitAddress(" ::ffff:203.0.113.7\n")
	if err != nil {
		t.Fatal(err)
	}
	if a.String() != "203.0.113.7" {
		t.Errorf("parsed %s", a)
	}
}

// The form offers UTC first and then each timezone once, in order, and
// every one is a timezone this Mac knows: a config that names one is
// valid.
func TestTheTimezonesOfferedAreTimezones(t *testing.T) {
	if len(Timezones) < 400 || Timezones[0] != "UTC" {
		t.Fatalf("%d timezones are offered, the first of them %q", len(Timezones), Timezones[0])
	}
	if !slices.IsSorted(Timezones[1:]) || len(slices.Compact(slices.Clone(Timezones))) != len(Timezones) {
		t.Error("the timezones offered are not in order, or one is offered twice")
	}
	for _, name := range Timezones {
		c := gated()
		c.Clock.Timezone = name
		if errs := c.Validate(); errs != nil {
			t.Errorf("%s: %v", name, errs)
		}
	}
	if slices.Contains(Timezones, TimezoneAuto) || slices.Contains(Timezones, "Local") {
		t.Error("the timezones offered hold a name that is no timezone's")
	}
}

func TestStatusString(t *testing.T) {
	tests := []struct {
		s    Status
		want string
	}{
		{Status{State: StateGateOpen, ExitAddress: "203.0.113.7"}, "GATE OPEN [203.0.113.7]"},
		{Status{State: StateGateLocked, LockReason: LockExitAddressMismatch, ExitAddress: "198.51.100.9"}, "GATE LOCKED [Exit Address Mismatch]"},
		{Status{State: StateStopped}, "STOPPED"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("the status reads %q, want %q", got, tt.want)
		}
	}
}

func TestParsePublicIPv4(t *testing.T) {
	if a, err := ParsePublicIPv4(" 1.1.1.1\n"); err != nil || a.String() != "1.1.1.1" {
		t.Errorf("1.1.1.1 parsed as %v, %v", a, err)
	}
	for _, s := range []string{"", "one.one.one.one", "::ffff:1.1.1.1", "2606:4700:4700::1111", "0.0.0.0", "127.0.0.1", "10.1.2.3", "100.64.0.1", "169.254.169.254", "172.30.0.2", "192.168.5.2", "198.18.0.1", "224.0.0.251", "255.255.255.255"} {
		if a, err := ParsePublicIPv4(s); err == nil {
			t.Errorf("%q is accepted as the public IPv4 address %s", s, a)
		}
	}
}

// A new sandbox's gate keeps a Hold, and a config that was saved without
// the word reads as one that keeps none.
func TestANewSandboxKeepsAHold(t *testing.T) {
	cfg := NewConfig("s1", "Research")
	if !cfg.Gate.Hold {
		t.Error("a new sandbox's gate keeps no Hold")
	}
	data, err := json.Marshal(cfg)
	if err != nil || !strings.Contains(string(data), `"hold":true`) {
		t.Fatalf("a new sandbox's config is saved as %s (%v)", data, err)
	}
	var saved Config
	if err := json.Unmarshal([]byte(strings.Replace(string(data), `,"hold":true`, "", 1)), &saved); err != nil || saved.Gate.Hold {
		t.Errorf("a config saved without a Hold reads as %+v (%v)", saved.Gate, err)
	}
}

// A sandbox has a gate, and a new one is valid but for what only its
// owner knows: what it leaves this Mac by. Nothing leaves unjudged.
func TestANewSandboxNamesNoWayOut(t *testing.T) {
	fresh := NewConfig("s1", "Fresh")
	if got := fields(fresh.Validate()); len(got) != 1 || got[0] != FieldAllowedInterfaces {
		t.Errorf("a new sandbox has errors on %v, want the Allowed Interfaces alone", got)
	}
	// Changing one sandbox's probe URLs changes no other's.
	fresh.Gate.ProbeURLs[0] = "https://probe.example.com/ip"
	if got := NewConfig("s2", "Next").Gate.ProbeURLs[0]; got != DefaultProbeURLs[0] || got == fresh.Gate.ProbeURLs[0] {
		t.Errorf("the next new sandbox starts with the probe URL %q", got)
	}
}
