package sandbox

import (
	"encoding/json"
	"strings"
	"testing"
)

// A heartbeat that does not say this Mac's physical exit addresses are known
// says they are not: the zero value, and a line written before the
// field was, fail closed.
func TestAHeartbeatThatDoesNotSayKnowsNothing(t *testing.T) {
	if (Heartbeat{}).PhysicalExitKnown {
		t.Error("the zero heartbeat says this Mac's physical exit addresses are known")
	}
	var h Heartbeat
	if err := json.Unmarshal([]byte(`{"hostConditionsMet":true,"interface":"en0","interfaceChange":1}`), &h); err != nil {
		t.Fatal(err)
	}
	if h.PhysicalExitKnown || len(h.PhysicalExitAddresses) != 0 {
		t.Errorf("a heartbeat that names none is read as %+v", h)
	}
	// And one that says nothing of them is the line it was before.
	line, err := json.Marshal(Heartbeat{HostConditionsMet: true, Interface: "en0", InterfaceChange: 1})
	if err != nil || strings.Contains(string(line), "own") {
		t.Errorf("a heartbeat with nothing to say of them is written %s (%v)", line, err)
	}
	// Known, with none: this Mac has no other way out than the Interface
	// in use.
	line, _ = json.Marshal(Heartbeat{PhysicalExitKnown: true})
	if err := json.Unmarshal(line, &h); err != nil || !h.PhysicalExitKnown || len(h.PhysicalExitAddresses) != 0 {
		t.Errorf("known with none is read back as %+v (%v)", h, err)
	}
}

// What an Allowed Interface expects is one of three, and a config saved
// before the third was there loads as it was.
func TestAnAllowedInterfaceExpectsOneOfThree(t *testing.T) {
	for _, tt := range []struct {
		ifc  AllowedInterface
		want int
	}{
		{AllowedInterface{Name: "en0"}, 0},
		{AllowedInterface{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7", "203.0.113.8"}}, 1},
		{AllowedInterface{Name: "en0", AnyExitAddress: true}, 1},
		{AllowedInterface{Name: "en0", AnyExitAddressButPhysical: true}, 1},
		{AllowedInterface{Name: "en0", AnyExitAddress: true, AnyExitAddressButPhysical: true}, 2},
		{AllowedInterface{Name: "en0", ExpectedExitAddresses: []string{"203.0.113.7"}, AnyExitAddress: true, AnyExitAddressButPhysical: true}, 3},
	} {
		if got := tt.ifc.Expectations(); got != tt.want {
			t.Errorf("%+v expects %d of the three, want %d", tt.ifc, got, tt.want)
		}
	}
	var saved AllowedInterface
	if err := json.Unmarshal([]byte(`{"name":"utun4","anyExitAddress":true}`), &saved); err != nil || saved.AnyExitAddressButPhysical || !saved.AnyExitAddress {
		t.Errorf("an Interface saved before is read as %+v (%v)", saved, err)
	}
	line, _ := json.Marshal(saved)
	if string(line) != `{"name":"utun4","anyExitAddress":true}` {
		t.Errorf("and written back as %s", line)
	}
}
