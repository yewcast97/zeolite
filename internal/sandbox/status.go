package sandbox

// State is what a sandbox is doing, as the glyph of its Card shows it.
type State string

const (
	StateStopped    State = "STOPPED"
	StateStarting   State = "STARTING"
	StateGateOpen   State = "GATE OPEN"
	StateGateLocked State = "GATE LOCKED"
	StateError      State = "ERROR"
)

// LockReason is why a gate is locked.
type LockReason string

const (
	// LockExitAddressMismatch: the probe answered with an address that is
	// not expected.
	LockExitAddressMismatch LockReason = "Exit Address Mismatch"
	// LockProbeUnanswered: the probe got no answer.
	LockProbeUnanswered LockReason = "Probe Unanswered"
	// LockHostCondition: a Host Condition is unmet.
	LockHostCondition LockReason = "Host Condition"
	// LockPhysicalExitUnknown: the Interface in use expects this Mac's
	// physical exit address, or any but it, and the app does not know this
	// Mac's physical exit address now.
	LockPhysicalExitUnknown LockReason = "Physical Exit Address Unknown"
	// LockNoInterface: none of the sandbox's Allowed Interfaces is up
	// with an address.
	LockNoInterface LockReason = "No Interface"
	// LockHeartbeatLost: the app stopped sending heartbeats.
	LockHeartbeatLost LockReason = "Heartbeat Lost"
)

// Status is a sandbox's state with what explains it.
type Status struct {
	State State
	// LockReason is set in StateGateLocked.
	LockReason LockReason
	// ExitAddress is the address the probe last saw, in StateGateOpen and
	// in StateGateLocked for a mismatch.
	ExitAddress string
	// Error is set in StateError.
	Error string
}

// String is the status in a line: the state, with what qualifies it in
// brackets.
func (s Status) String() string {
	switch {
	case s.State == StateGateOpen && s.ExitAddress != "":
		return string(s.State) + " [" + s.ExitAddress + "]"
	case s.State == StateGateLocked && s.LockReason != "":
		return string(s.State) + " [" + string(s.LockReason) + "]"
	}
	return string(s.State)
}
