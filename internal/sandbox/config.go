// Package sandbox holds the core types of zeolite: what a sandbox is
// configured to be and what state it is in. It imports nothing else of
// the app; everything else imports it.
package sandbox

import "regexp"

// Config is everything the user sets for one sandbox.
type Config struct {
	// ID names the sandbox's containers and network. It never changes.
	ID   string `json:"id"`
	Name string `json:"name"`

	// AllowedInterfaces are the Interfaces the sandbox may leave this Mac
	// by, most preferred first. It leaves by the first that is up with an
	// address, and by no other: TCP and name queries, and only while the
	// probe sees an exit address expected there.
	AllowedInterfaces []AllowedInterface `json:"allowedInterfaces,omitempty"`
	Gate              GateConfig         `json:"gate"`
	// HostConditions lock the gate while any of them is unmet.
	HostConditions []HostCondition `json:"hostConditions,omitempty"`

	Clock     ClockPolicy `json:"clock"`
	Resources Resources   `json:"resources"`

	// Packs are the names of the enabled Capability Packs.
	Packs []string `json:"packs,omitempty"`
	// SharedFolder names the Shared Folder the sandbox is given, empty
	// for none: a folder of the Mac, mounted in its workstation at
	// SharedFolderPath. Sandboxes that name the same one mount the same
	// folder.
	SharedFolder string `json:"sharedFolder,omitempty"`

	// HostTools says whether the sandbox's agent may run Host Tools, and
	// what their Wall leaves them besides what it leaves every one.
	HostTools HostTools `json:"hostTools"`
}

// HostTools is what a sandbox is given of this Mac's programs. They run
// in its Shared Folder: a sandbox that is given none has no Host Tools.
type HostTools struct {
	// Enabled says that the sandbox's agent may run Host Tools.
	Enabled bool `json:"enabled,omitempty"`
	// GPU adds this Mac's graphics device to the Host Tool Wall, with the
	// system's shader compiler and its hardware video encoder and
	// decoder: what Metal and VideoToolbox need.
	GPU bool `json:"gpu,omitempty"`
	// MacName adds this Mac's name to the Host Tool Wall: uname fails
	// without it, and with it what asks a platform's name, as Python's
	// platform module does. The name often holds its owner's.
	MacName bool `json:"macName,omitempty"`
	// Applications are the sandbox's Allowed Applications: folders of
	// this Mac's /Applications, by name, that its Host Tools read and
	// run. Of any other the Host Tool Wall says nothing, not even
	// whether it is there.
	Applications []string `json:"applications,omitempty"`
}

// ReservedEnvironment are the variables the app sets for every
// workstation. A Capability Pack may not set them: that would undo the
// sandbox's Clock Policy, what its supervisor is told, or where an
// agent is sent for the sandbox's MCP servers.
var ReservedEnvironment = reservedEnvironment()

func reservedEnvironment() map[string]bool {
	reserved := map[string]bool{"TZ": true, SupervisorConfigEnv: true}
	// Where the app sends an agent for a registry of its own.
	for variable := range RegistryEnvironment() {
		reserved[variable] = true
	}
	return reserved
}

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvironmentName reports whether name can name an environment variable.
func EnvironmentName(name string) bool { return environmentName.MatchString(name) }

// AllowedInterface is one Interface a sandbox may leave this Mac by,
// with what the probe may see while it does.
type AllowedInterface struct {
	// Name is the Interface's name on this Mac: en0, utun4.
	Name string `json:"name"`
	// ExpectedExitAddresses are the addresses the probe may see while the
	// sandbox leaves by this Interface: public IPv4 addresses.
	ExpectedExitAddresses []string `json:"expectedExitAddresses,omitempty"`
	// AnyExitAddress says that whatever address the probe sees there
	// will do. It is said, never assumed: an Interface that expects
	// nothing is an error.
	AnyExitAddress bool `json:"anyExitAddress,omitempty"`
	// AnyExitAddressButPhysical says that any IPv4 address the probe sees
	// there will do but one of this Mac's physical exit addresses: where an
	// Interface that stays up leads once it stops leading elsewhere. The
	// Host Watchdog reads those, and the gate is locked while it does not
	// know them.
	AnyExitAddressButPhysical bool `json:"anyExitAddressButPhysical,omitempty"`
	// PhysicalExitAddress says that the probe must see one of this Mac's
	// physical exit addresses there: the sandbox exits where this Mac's
	// physical Interfaces do, with no tunnel's far end between. The Host
	// Watchdog reads those, the Interface in use's among them where it is
	// one of this Mac's physical Interfaces, and the gate is locked while
	// it does not know them.
	PhysicalExitAddress bool `json:"physicalExitAddress,omitempty"`
	// StandInAddresses says that what this Interface leads to answers
	// names with stand-in addresses. The sandbox then connects to those
	// too while it leaves by this Interface, and by no other: where each
	// leads is decided where the Interface leads, by the name it stands
	// for, and not by its being a public address.
	StandInAddresses bool `json:"standInAddresses,omitempty"`
}

// Expectations counts what the Interface expects of the four: listed
// exit addresses, any, any but physical, physical. A config is valid
// with exactly one.
func (a AllowedInterface) Expectations() int {
	n := 0
	for _, said := range []bool{len(a.ExpectedExitAddresses) > 0, a.AnyExitAddress, a.AnyExitAddressButPhysical, a.PhysicalExitAddress} {
		if said {
			n++
		}
	}
	return n
}

// GateConfig says how a sandbox's exit address is read.
type GateConfig struct {
	// ProbeURLs are HTTPS endpoints that answer with the caller's address
	// as plain text. They are asked together: the first to answer with an
	// address that is expected at the Interface in use keeps the gate
	// open, and one that answers with another address locks it. The Host
	// Watchdog asks the same ones for this Mac's physical exit addresses.
	ProbeURLs []string `json:"probeURLs"`
	// ProbePin, when set, is the base64 SHA-256 of the public key the
	// probe endpoints must present.
	ProbePin string `json:"probePin,omitempty"`
	// Resolver is the public IPv4 address of the one DNS resolver the
	// sandbox's name queries go to.
	Resolver string `json:"resolver"`
	// Hold says that the sandbox's gate keeps a Hold: the connections
	// the sandbox has are kept for HoldDuration after the last probe
	// that was answered with an expected address was sent, while no
	// probe is answered. What it sends on them waits at its Carrier
	// then, and it makes no new one and asks no name.
	Hold bool `json:"hold,omitempty"`
}

// DefaultResolver is the resolver a new sandbox starts with.
const DefaultResolver = "1.1.1.1"

// DefaultProbeURLs are the probe URLs a new sandbox starts with: three
// endpoints that three operators run, so that the outage of one leaves
// two to answer.
var DefaultProbeURLs = []string{
	"https://icanhazip.com",
	"https://ifconfig.me/ip",
	"https://checkip.amazonaws.com",
}

// HostConditionKind is what a Host Condition checks on the Mac.
type HostConditionKind string

// WiFiSSID holds while the Mac is joined to the Wi-Fi network named Value.
const WiFiSSID HostConditionKind = "wifi_ssid"

// HostConditionKinds lists the kinds in the order the dashboard offers them.
var HostConditionKinds = []HostConditionKind{WiFiSSID}

// HostCondition is a fact about the Mac, other than its interfaces, that
// must hold for egress. What interfaces a sandbox may use is said by its
// Allowed Interfaces.
type HostCondition struct {
	Kind  HostConditionKind `json:"kind"`
	Value string            `json:"value"`
}

// TimezoneAuto is the ClockPolicy.Timezone that takes the timezone of the
// exit address.
const TimezoneAuto = "auto"

// ClockPolicy is the timezone a sandbox's clock reads in. The clock
// itself is this Mac's: nothing shifts it.
type ClockPolicy struct {
	// Timezone is an IANA name such as "Europe/Berlin", or TimezoneAuto.
	Timezone string `json:"timezone"`
	// TimezoneProbeURL is an HTTPS endpoint that answers with the IANA
	// timezone of the caller's address, read when Timezone is
	// TimezoneAuto.
	TimezoneProbeURL string `json:"timezoneProbeURL,omitempty"`
}

// Resources are the limits of the workstation container.
type Resources struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memoryMB"`
	// ShmMB is the size of /dev/shm, which counts against MemoryMB.
	ShmMB int `json:"shmMB"`
}

// MinShmMB is the least /dev/shm a workstation gets: Chromium and 3D
// engines crash with less.
const MinShmMB = 2048

// NewConfig returns a config with the defaults a new sandbox starts
// from. Its gate keeps a Hold: a lease that runs out there costs a wait
// and no connection. A sandbox that was saved without one keeps none
// until its Setup says so.
func NewConfig(id, name string) Config {
	return Config{
		ID:        id,
		Name:      name,
		Gate:      GateConfig{ProbeURLs: append([]string(nil), DefaultProbeURLs...), Resolver: DefaultResolver, Hold: true},
		Clock:     ClockPolicy{Timezone: "UTC"},
		Resources: Resources{CPUs: 2, MemoryMB: 4096, ShmMB: MinShmMB},
	}
}
