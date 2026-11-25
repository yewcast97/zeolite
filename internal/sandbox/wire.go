package sandbox

import "time"

// The types in this file are what the app and the binaries it starts
// say to each other, as JSON lines: the app writes to the gatekeeper's
// standard input, and reads the gatekeeper's and the supervisor's
// standard output from the container logs; and it writes to a Carrier
// and reads from it on two descriptors of their own.

// GatekeeperConfig is the first line the app writes to a gatekeeper.
type GatekeeperConfig struct {
	// Resolver is the one DNS resolver the sandbox's name queries, and the
	// probe's, go to.
	Resolver string `json:"resolver"`
	// AllowedInterfaces say, for each Interface the sandbox may leave
	// by, what the probe may see while it does.
	AllowedInterfaces []AllowedInterface `json:"allowedInterfaces,omitempty"`
	ProbeURLs         []string           `json:"probeURLs,omitempty"`
	ProbePin          string             `json:"probePin,omitempty"`
	// Hold says that the gate keeps a Hold: the ruleset then has its
	// rule and its set, and has neither otherwise.
	Hold bool `json:"hold,omitempty"`
	// TimezoneProbeURL is set when the Clock Policy takes the timezone
	// of the exit address: the gatekeeper reads it and reports it in
	// GateReady.
	TimezoneProbeURL string `json:"timezoneProbeURL,omitempty"`
}

// Heartbeat is every later line the app writes to a gatekeeper. The
// lease lives only while heartbeats arrive; the end of the stream locks
// the gate for good.
type Heartbeat struct {
	// HostConditionsMet is whether every Host Condition of the sandbox
	// holds on the Mac now.
	HostConditionsMet bool `json:"hostConditionsMet"`
	// Interface is the Interface in use, as the sandbox's Carrier last
	// reported it, empty when there is none, and InterfaceChange the
	// count of its changes. A pair the gatekeeper has not seen ends the
	// lease: nothing crosses a new Interface under the lease that was
	// kept for the one before.
	Interface       string `json:"interface,omitempty"`
	InterfaceChange uint64 `json:"interfaceChange"`
	// PhysicalExitAddresses are this Mac's physical exit addresses as the
	// Host Watchdog reads them now, and PhysicalExitKnown whether it has
	// read them, lately, from every one of this Mac's physical Interfaces
	// that counts: all of them where the Interface in use expects this
	// Mac's physical exit address, and all but the Interface in use where
	// it expects any but physical. The app says so only while the Interface
	// in use expects one of the two. A heartbeat that does not say they are
	// known says they are not, and a gate that needs them is locked.
	PhysicalExitAddresses []string `json:"physicalExitAddresses,omitempty"`
	PhysicalExitKnown     bool     `json:"physicalExitKnown,omitempty"`
}

// HeartbeatInterval is how often the app sends a Heartbeat, and
// LeaseDuration how long the gate stays open after the last probe that
// was answered with an expected address was sent. HoldDuration is how
// long after that probe a Hold lasts: the connections the sandbox has
// by then are kept that long with no answer, where its Setup asks for a
// Hold, and what they send waits at the Carrier.
//
// HoldLeaseMargin is what a gate that keeps a Hold adds, for its lease,
// to the time in which the next two answers are due: there the lease
// runs from an answer until two more were due and that much longer, so
// that it outlasts one probe that is lost and an answer that takes
// longer than the one before; and never longer than LeaseDuration after
// the probe was sent. So what the sandbox sends leaves for little
// longer than an answer takes after the last probe that vouched for
// where it leaves, by a way out that answers fast; and by one that
// answers slowly the gate still opens. A lease that runs out there
// costs no connection, only a wait. The margin was chosen from what
// integration/leasetrace_test.go counted on two ways out.
// Everything else that is counted in a lease's duration is counted in
// LeaseDuration at every gate: how long an answer with an address that
// is not expected is believed, how long a gate goes without a
// heartbeat, how long a Carrier without a judgement.
const (
	HeartbeatInterval = 1500 * time.Millisecond
	LeaseDuration     = 5 * time.Second
	HoldLeaseMargin   = 750 * time.Millisecond
	HoldDuration      = 30 * time.Second
)

// The Probe Source Ports are the ports the gatekeeper's probes are sent
// from. The ruleset rejects what comes from one of them without the
// probe's mark, so a connection from one is a probe's: by them the
// Carrier, which no mark reaches, knows a probe's connection from the
// sandbox's own. They lie above the ports a namespace hands out by
// itself.
const (
	ProbeSourcePortFirst = 61000
	ProbeSourcePortLast  = 61999
)

// ProbeSourcePort is whether a port is one of the Probe Source Ports.
func ProbeSourcePort(port uint16) bool {
	return port >= ProbeSourcePortFirst && port <= ProbeSourcePortLast
}

// GateReport is a line a gatekeeper writes: exactly one field is set.
type GateReport struct {
	Ready *GateReady     `json:"ready,omitempty"`
	Gate  *GateTelemetry `json:"gate,omitempty"`
	// Error is a failure the gatekeeper cannot recover from; it exits
	// after reporting it.
	Error string `json:"error,omitempty"`
}

// GateReady says that the ruleset is loaded.
type GateReady struct {
	// RulesetHash identifies the loaded ruleset, as `gatekeeper verify`
	// reports it for the live one.
	RulesetHash string `json:"rulesetHash"`
	// Timezone is the IANA timezone of the exit address, when the config
	// asked.
	Timezone string `json:"timezone,omitempty"`
}

// GateTelemetry is the state of a gate at one moment.
type GateTelemetry struct {
	Open bool `json:"open"`
	// LockReason is set while the gate is locked.
	LockReason LockReason `json:"lockReason,omitempty"`
	// LeaseFor is how long the lease has to run, when this is written,
	// while the gate is open.
	LeaseFor time.Duration `json:"leaseFor,omitempty"`
	// Held is whether the Hold lasts while the gate is locked: the
	// connections the sandbox has are kept, what they send waits, and it
	// makes no new one.
	Held bool `json:"held,omitempty"`
	// Interface and InterfaceChange are the Interface in use and the
	// count of its changes that the gatekeeper has taken note of: the
	// lease kept before that change is ended.
	Interface       string `json:"interface,omitempty"`
	InterfaceChange uint64 `json:"interfaceChange"`
	// ExitAddress is what the last round of probes answered first, or
	// the address that was not expected while the gate is locked for one.
	ExitAddress string `json:"exitAddress,omitempty"`
	// ProbeLatency is how long that answer took.
	ProbeLatency time.Duration `json:"probeLatency,omitempty"`
	// PhysicalExitAddresses and PhysicalExitKnown are this Mac's physical
	// exit addresses as the gatekeeper last took note of them from a
	// heartbeat, and whether it took them for known.
	PhysicalExitAddresses []string `json:"physicalExitAddresses,omitempty"`
	PhysicalExitKnown     bool     `json:"physicalExitKnown,omitempty"`
	// PacketsPassed and PacketsRejected count the sandbox's egress
	// packets since the gatekeeper started.
	PacketsPassed   uint64 `json:"packetsPassed"`
	PacketsRejected uint64 `json:"packetsRejected"`
}

// CarrierConfig is the first line the app writes to a Carrier.
type CarrierConfig struct {
	// AllowedInterfaces are the names of the Interfaces the sandbox may
	// leave this Mac by, most preferred first.
	AllowedInterfaces []string `json:"allowedInterfaces"`
	// Resolver is the one resolver the sandbox's name queries are
	// carried to.
	Resolver string `json:"resolver"`
	// ExpectedExitAddresses are the addresses the sandbox is expected to
	// be seen at, by any of its Interfaces. The Carrier carries nothing
	// to them: a sandbox has no business with its own way out.
	ExpectedExitAddresses []string `json:"expectedExitAddresses,omitempty"`
	// StandInInterfaces are the names of the Allowed Interfaces where
	// names are answered with stand-in addresses: the Carrier carries to
	// those too while one of them is the Interface in use.
	StandInInterfaces []string `json:"standInInterfaces,omitempty"`
}

// CarrierJudgement is every later line the app writes to a Carrier: what
// the gatekeeper last said of the gate, and the Interface in use the
// gatekeeper has taken note of.
type CarrierJudgement struct {
	// Open is whether the gate is open, and LeaseFor how long its lease
	// had to run when the gatekeeper said so: the Carrier sends on what
	// the sandbox sends for that long from when it is told, and no
	// longer without another judgement.
	Open     bool          `json:"open"`
	LeaseFor time.Duration `json:"leaseFor,omitempty"`
	// Held is whether the Hold lasts while the gate is locked: the
	// Carrier keeps the connections it has, and sends on nothing the
	// sandbox sends on them until the gate is open again.
	Held bool `json:"held,omitempty"`
	// Interface and InterfaceChange are the Interface in use and the
	// count of its changes that the gatekeeper acknowledged: it has ended
	// the lease that was kept under the one before.
	Interface       string `json:"interface,omitempty"`
	InterfaceChange uint64 `json:"interfaceChange"`
}

// CarrierReport is a line a Carrier writes: the Interface in use, as it
// last changed.
type CarrierReport struct {
	// Interface is the Interface in use, empty when there is none.
	Interface string `json:"interface,omitempty"`
	// InterfaceChange counts the changes of the Interface in use since
	// the Carrier started. The first Interface in use is change 1.
	InterfaceChange uint64 `json:"interfaceChange"`
}

// SupervisorConfig tells a workstation's supervisor what to run. It
// travels in the environment variable SupervisorConfigEnv.
type SupervisorConfig struct {
	Daemons    []Daemon    `json:"daemons,omitempty"`
	MCPServers []MCPServer `json:"mcpServers,omitempty"`
	// HostTools says that the sandbox is given Host Tools: the
	// supervisor then registers the server by which the agent runs them,
	// whose tool says what their Wall was given.
	HostTools *HostToolsNotice `json:"hostTools,omitempty"`
	// SharedFolder says that the workstation mounts a Shared Folder, at
	// SharedFolderPath.
	SharedFolder bool `json:"sharedFolder,omitempty"`
}

// HostToolsNotice is what a workstation is told of its sandbox's Host
// Tools, for what their tool says of itself: what their Wall was given.
type HostToolsNotice struct {
	GPU     bool `json:"gpu,omitempty"`
	MacName bool `json:"macName,omitempty"`
}

// SupervisorConfigEnv names the variable that holds a SupervisorConfig.
const SupervisorConfigEnv = "ZEOLITE_SUPERVISOR"

// Daemon is a Capability Pack's DaemonProcess that runs in the sandbox.
type Daemon struct {
	Pack          string   `json:"pack"`
	Command       []string `json:"command"`
	ReadinessPort int      `json:"readinessPort"`
}

// MCPServer is a Capability Pack's MCPBridge.
type MCPServer struct {
	Name    string            `json:"name"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
}

// SupervisorReport is a line a supervisor writes: exactly one field is set.
type SupervisorReport struct {
	Status  *WorkstationStatus `json:"status,omitempty"`
	MCPCall *MCPCall           `json:"mcpCall,omitempty"`
}

// WorkstationStatus is what runs in a workstation at one moment.
type WorkstationStatus struct {
	// Clock is the time as programs in the sandbox see it: this Mac's,
	// in RFC 3339 with the timezone of the sandbox's Clock Policy.
	Clock   string        `json:"clock"`
	Daemons []DaemonState `json:"daemons,omitempty"`
	// Sessions are the Terminal Sessions that have a process left, each
	// with the agent that runs in it, if one does.
	Sessions []SessionState `json:"sessions,omitempty"`
	// MemoryUsed is the memory the workstation has in use, in bytes, and
	// MemoryLimit the most it may have: the sandbox's memory, as the
	// kernel accounts for the container, with what it holds of files and
	// gives back when asked left out. Both are zero where the workstation
	// could not read them.
	MemoryUsed  uint64 `json:"memoryUsed,omitempty"`
	MemoryLimit uint64 `json:"memoryLimit,omitempty"`
}

// SessionState is what runs in a Terminal Session at one moment, as the
// supervisor reads it from the kernel's list of processes. It reads
// nothing the session printed.
type SessionState struct {
	// ID is the session's ID in its workstation.
	ID string `json:"id"`
	// Agent is the Agent Executable of the agent among the session's
	// processes, empty where there is none: a shell, and what else its
	// user started there.
	Agent string `json:"agent,omitempty"`
}

// DaemonState is whether a pack's daemon is up.
type DaemonState struct {
	Pack  string `json:"pack"`
	Ready bool   `json:"ready"`
	// Error is why it is not, when the supervisor knows.
	Error string `json:"error,omitempty"`
}

// MCPCall is one JSON-RPC message that crossed an MCP bridge.
type MCPCall struct {
	Time   time.Time `json:"time"`
	Server string    `json:"server"`
	// FromAgent is true for a message the agent sent to the server.
	FromAgent bool   `json:"fromAgent"`
	Method    string `json:"method,omitempty"`
	// Summary is the start of the message's params, result or error.
	Summary string `json:"summary,omitempty"`
}

// Update is news about one running sandbox, as the orchestrator tells it
// and the dashboard shows it: exactly one of its pointers is set.
type Update struct {
	SandboxID   string
	Status      *Status
	Gate        *GateTelemetry
	Workstation *WorkstationStatus
	MCPCall     *MCPCall
	// Hand says that the agent of a Terminal Session raised its hand.
	// It is an event: nothing lowers a hand but its user's look.
	Hand *Hand
}

// Hand is a raised hand: the Terminal Session whose agent cannot go on
// without its user.
type Hand struct {
	// Session is the session's ID in its workstation.
	Session string
}
