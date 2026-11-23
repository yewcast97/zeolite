// Package hostwatch is the Host Watchdog: it evaluates Host Conditions,
// facts about the Mac that a sandbox's egress depends on, it reads this
// Mac's physical exit addresses for the sandboxes that expect those, or any
// exit address but those, and it names this Mac's interfaces for
// whoever configures a sandbox's Allowed Interfaces.
//
// It is the one part of the app's process that opens a connection, and
// only to read this Mac's physical exit addresses: each on a socket bound to
// one of this Mac's physical Interfaces, to a probe URL or to the sandbox's
// resolver.
package hostwatch

import (
	"net"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"zeolite/internal/sandbox"
)

// Watchdog evaluates Host Conditions, and reads this Mac's physical
// exit addresses. One is shared by every sandbox: each asks for
// every heartbeat, and the Mac's Wi-Fi networks are read at most
// once in maxAge whoever asks.
type Watchdog struct {
	// readSSIDs returns the names of the Wi-Fi networks the Mac is
	// joined to.
	readSSIDs func() []string

	mu     sync.Mutex
	ssids  []string
	readAt time.Time

	// physical reads this Mac's physical exit addresses, under a lock of
	// its own: mu is held while ipconfig runs.
	physical *physicalExits
}

// maxAge is how old a reading of the Mac may be. It is well under the
// heartbeat's interval, so a condition that fails is acted on by the
// next heartbeat.
const maxAge = time.Second

// New returns a Watchdog that reads this Mac.
func New() *Watchdog {
	return &Watchdog{readSSIDs: joinedSSIDs, physical: newPhysicalExits()}
}

// Met reports whether every condition holds now. A condition that cannot
// be read counts as unmet: not knowing is no reason to let traffic out.
func (w *Watchdog) Met(conditions []sandbox.HostCondition) bool {
	for _, c := range conditions {
		if !w.met(c) {
			return false
		}
	}
	return true
}

func (w *Watchdog) met(c sandbox.HostCondition) bool {
	switch c.Kind {
	case sandbox.WiFiSSID:
		for _, ssid := range w.currentSSIDs() {
			if ssid == c.Value {
				return true
			}
		}
	}
	return false
}

func (w *Watchdog) currentSSIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if time.Since(w.readAt) > maxAge {
		w.ssids, w.readAt = w.readSSIDs(), time.Now()
	}
	return w.ssids
}

var ssidLine = regexp.MustCompile(`(?m)^\s*SSID : (.+)$`)

// joinedSSIDs asks `ipconfig getsummary`, which reports the network
// name on recent macOS, where `networksetup` does not.
func joinedSSIDs() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ssids []string
	for _, iface := range interfaces {
		if !strings.HasPrefix(iface.Name, "en") || iface.Flags&net.FlagUp == 0 {
			continue
		}
		out, err := exec.Command("ipconfig", "getsummary", iface.Name).Output()
		if err != nil {
			continue
		}
		if ssid, ok := parseSSID(string(out)); ok {
			ssids = append(ssids, ssid)
		}
	}
	return ssids
}

// parseSSID reads the network name from `ipconfig getsummary`. A name
// macOS withholds, which it prints as "<redacted>", is no name.
func parseSSID(summary string) (string, bool) {
	m := ssidLine.FindStringSubmatch(summary)
	if m == nil || m[1] == "<redacted>" {
		return "", false
	}
	return m[1], true
}

// Interface is one of this Mac's network interfaces that a sandbox
// could leave by now.
type Interface struct {
	// Name is the interface's name, which is what an Allowed Interface
	// holds: en0.
	Name string
	// Title is what the system calls the interface's port, as the menu
	// bar and System Settings do: Wi-Fi. A tunnel has none.
	Title string
}

// Interfaces lists the interfaces of this Mac that a sandbox could leave
// by at this moment: those that are up with an IPv4 address and lead
// somewhere, through a router or, as a tunnel does, to one other end.
// Loopback and a host-only bridge lead nowhere and are left out. It
// answers at once: the system's titles are read beside it, and are
// there from the second time it is asked.
func Interfaces() []Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	routers, _ := defaultRouters()
	titles := ports.titles()
	var list []Interface
	for _, ifi := range all {
		_, routed := routers[ifi.Index]
		if !sandbox.ValidInterfaceName(ifi.Name) || ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 || ipv4Of(ifi) == "" {
			continue
		}
		if !routed && ifi.Flags&net.FlagPointToPoint == 0 {
			continue
		}
		list = append(list, Interface{Name: ifi.Name, Title: titles[ifi.Name]})
	}
	// The ports the system names come first, as it orders its own list.
	sort.SliceStable(list, func(i, j int) bool { return list[i].Title != "" && list[j].Title == "" })
	return list
}

// ipv4Of is an interface's first IPv4 address, or empty.
func ipv4Of(ifi net.Interface) string {
	addrs, err := ifi.Addrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ipnet.IP.To4().String()
		}
	}
	return ""
}

// portsMaxAge is how old the system's titles may be: a port is added or
// renamed when hardware is plugged in or a setting is changed, by hand.
const portsMaxAge = 10 * time.Second

// portTitles holds what the system calls each interface's port. Reading
// it runs a program, so it is read beside whoever asks, at most once in
// portsMaxAge.
type portTitles struct {
	mu      sync.Mutex
	known   map[string]string
	read    time.Time
	reading bool
}

var ports portTitles

// titles returns the titles last read, and reads them again beside the
// caller when they are older than portsMaxAge.
func (p *portTitles) titles() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.reading && time.Since(p.read) > portsMaxAge {
		p.reading = true
		go func() {
			out, err := exec.Command("networksetup", "-listallhardwareports").Output()
			p.mu.Lock()
			defer p.mu.Unlock()
			if err == nil {
				p.known = parsePorts(string(out))
			}
			p.read, p.reading = time.Now(), false
		}()
	}
	return p.known
}

var portLine = regexp.MustCompile(`(?m)^Hardware Port: (.+)\nDevice: (\S+)$`)

// parsePorts reads `networksetup -listallhardwareports`: for each
// interface that is a hardware port, what the system calls the port.
func parsePorts(listing string) map[string]string {
	titles := map[string]string{}
	for _, m := range portLine.FindAllStringSubmatch(listing, -1) {
		titles[m[2]] = strings.TrimSpace(m[1])
	}
	return titles
}
