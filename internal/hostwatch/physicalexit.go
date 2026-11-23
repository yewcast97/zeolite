package hostwatch

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"zeolite/internal/probe"
	"zeolite/internal/sandbox"
)

// This Mac's physical exit addresses are read here, for the sandboxes whose
// Interface in use expects those, or any exit address but those: the
// Host Watchdog
// asks each sandbox's probe URLs, as its gatekeeper asks them, on
// sockets bound to each of this Mac's physical Interfaces. Nothing is asked
// while no sandbox asks for them.

const (
	// physicalExitEvery is how long after a Round by one of this Mac's
	// physical Interfaces the next is sent, and physicalExitRetry how long
	// after one while the interface has no answer that is believed: an
	// interface that is not known keeps a gate locked, and is asked again
	// sooner.
	physicalExitEvery = 10 * time.Second
	physicalExitRetry = 2 * time.Second
	// physicalExitBelieved is how long an answer is believed, counted from
	// when its probe was sent: every address an interface answered with in
	// that time is one of this Mac's physical exit addresses, so that a
	// network that leaves from a few addresses is covered. It is also how
	// long what nobody asked for is kept.
	physicalExitBelieved = 60 * time.Second
)

// physicalInterface is one of this Mac's physical Interfaces as it was
// when looked at: up, with an IPv4 address, a hardware address and a
// default route through a router. What it answered belongs to it as it
// was: when anything here changes, it is another.
type physicalInterface struct {
	Name  string
	Index int
	// Addrs are its IPv4 addresses, in the order the system lists them.
	Addrs string
	// Router is where its default route goes.
	Router netip.Addr
}

// seenInterface is an interface of this Mac as the system lists it.
type seenInterface struct {
	Name     string
	Index    int
	Up       bool
	Hardware bool
	// Addrs are its IPv4 addresses.
	Addrs []netip.Addr
}

// physicalOf picks this Mac's physical Interfaces: those that are up with
// an IPv4 address, have a hardware address, and have a default route
// through a router, by the routers of the routing table. A tunnel has no
// hardware address; loopback has neither; a host-only bridge has no
// router. An interface with no such route has no exit of its own to read.
func physicalOf(seen []seenInterface, routers map[int]netip.Addr) []physicalInterface {
	var physical []physicalInterface
	for _, s := range seen {
		router, routed := routers[s.Index]
		if !s.Up || !s.Hardware || len(s.Addrs) == 0 || !routed {
			continue
		}
		addrs := make([]string, len(s.Addrs))
		for i, a := range s.Addrs {
			addrs[i] = a.String()
		}
		physical = append(physical, physicalInterface{Name: s.Name, Index: s.Index, Addrs: strings.Join(addrs, " "), Router: router})
	}
	return physical
}

// lookAtPhysicalInterfaces reads this Mac's interfaces and its routing table,
// and returns its physical Interfaces.
func lookAtPhysicalInterfaces() ([]physicalInterface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	routers, err := defaultRouters()
	if err != nil {
		return nil, err
	}
	seen := make([]seenInterface, 0, len(all))
	for _, ifi := range all {
		s := seenInterface{Name: ifi.Name, Index: ifi.Index, Up: ifi.Flags&net.FlagUp != 0, Hardware: len(ifi.HardwareAddr) > 0}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(ipn.IP.To4()); ok {
					s.Addrs = append(s.Addrs, ip)
				}
			}
		}
		seen = append(seen, s)
	}
	return physicalOf(seen, routers), nil
}

// asker sends a probe by one of this Mac's physical Interfaces.
type asker interface {
	// ExitAddress asks a probe URL what address it was called from.
	ExitAddress(ctx context.Context, url string) (netip.Addr, error)
}

// boundAsker makes the asker of one of this Mac's physical Interfaces, with a
// sandbox's gate settings: a probe.Client whose every socket is bound to
// the interface.
func boundAsker(ifc physicalInterface, gate sandbox.GateConfig) (asker, error) {
	resolver, err := sandbox.ParsePublicIPv4(gate.Resolver)
	if err != nil {
		return nil, fmt.Errorf("the resolver %q: %w", gate.Resolver, err)
	}
	client, err := probe.New(resolver, gate.ProbePin, boundControl(ifc.Index))
	if err != nil {
		return nil, err
	}
	return client, nil
}

// physicalExits reads this Mac's physical exit addresses. Its lock is its
// own: it is never held while the Mac is asked anything that takes time.
type physicalExits struct {
	// look returns this Mac's physical Interfaces as they are now.
	look func() ([]physicalInterface, error)
	// newAsker makes the asker of an interface and a gate's settings.
	newAsker func(physicalInterface, sandbox.GateConfig) (asker, error)
	// now is the wall clock: an answer ages while the Mac sleeps.
	now func() time.Time
	// say writes a line to the app's log.
	say func(string)

	mu sync.Mutex
	// changed is told when a physical exit address was read that was not
	// believed before: whoever asked may ask again at once.
	changed func()
	// epoch counts the times everything was forgotten: an answer to a
	// probe sent before the last is thrown away.
	epoch uint64
	// readings are what was read, by a gate's settings and an
	// interface's name. Sandboxes with the same settings share one.
	readings map[string]*reading
	// unreadable is why this Mac's physical Interfaces could not be told
	// when last looked at, as it was last said.
	unreadable string
}

// reading is what one of this Mac's physical Interfaces answered to the probe
// URLs of one gate's settings.
type reading struct {
	ifc   physicalInterface
	urls  []string
	asker asker
	// noAsker is why there is no asker.
	noAsker error
	// asking is true while a Round is under way, and sent when the last
	// was sent.
	asking bool
	sent   time.Time
	// answers are the addresses answered, each with when the latest
	// probe so answered was sent.
	answers map[netip.Addr]time.Time
	// wanted is when a sandbox last asked for this reading.
	wanted time.Time
	// silent is true once it was said that nothing answers.
	silent bool
}

func newPhysicalExits() *physicalExits {
	return &physicalExits{
		look:     lookAtPhysicalInterfaces,
		newAsker: boundAsker,
		// Without the monotonic reading, which stands still while the Mac
		// sleeps.
		now:      func() time.Time { return time.Now().Round(0) },
		say:      func(line string) { log.Print(line) },
		changed:  func() {},
		readings: map[string]*reading{},
	}
}

// settingsKey names a gate's settings: what a probe is asked with.
func settingsKey(gate sandbox.GateConfig) string {
	return strings.Join(gate.ProbeURLs, "\n") + "\x00" + gate.ProbePin + "\x00" + gate.Resolver
}

// addresses returns this Mac's physical exit addresses, as the probe URLs
// of a gate's settings see them, and whether they are known: this Mac has a
// physical Interface, and each that counts answered within
// physicalExitBelieved. beside names the one interface that does not count,
// though this Mac is known to have a physical Interface by it too: the
// Interface in use of a sandbox that expects any exit address but a
// physical one. It is empty where every physical Interface counts: for a
// sandbox that expects this Mac's physical exit address. It never waits: a
// Round that is due is sent beside it.
func (o *physicalExits) addresses(beside string, gate sandbox.GateConfig) (addrs []string, known bool) {
	physical, err := o.look()
	now := o.now()

	o.mu.Lock()
	// What nobody asked for is dropped.
	for key, r := range o.readings {
		if age := now.Sub(r.wanted); age > physicalExitBelieved || age < 0 {
			delete(o.readings, key)
		}
	}
	unreadable := ""
	switch {
	case err != nil:
		unreadable = "this Mac's physical Interfaces could not be read: " + err.Error()
	case len(physical) == 0:
		unreadable = "this Mac has no physical Interface: none that is up with an IPv4 address, a hardware address and a default route through a router"
	}
	say := unreadable != "" && unreadable != o.unreadable
	o.unreadable = unreadable
	if unreadable != "" {
		o.mu.Unlock()
		if say {
			o.say(unreadable)
		}
		return nil, false
	}

	known = true
	found := map[netip.Addr]bool{}
	settings := settingsKey(gate)
	for _, ifc := range physical {
		if ifc.Name == beside {
			continue
		}
		key := settings + "\x00" + ifc.Name
		r := o.readings[key]
		if r == nil || r.ifc != ifc {
			// New, or no longer the interface that answered: what it was
			// answered then says nothing of it now.
			r = &reading{ifc: ifc, urls: slices.Clone(gate.ProbeURLs), answers: map[netip.Addr]time.Time{}}
			r.asker, r.noAsker = o.newAsker(ifc, gate)
			o.readings[key] = r
		}
		r.wanted = now
		believed := false
		for addr, at := range r.answers {
			if age := now.Sub(at); age > physicalExitBelieved || age < 0 {
				delete(r.answers, addr)
				continue
			}
			believed = true
			found[addr] = true
		}
		if !believed {
			known = false
		}
		since := now.Sub(r.sent)
		if !r.asking && (r.sent.IsZero() || since >= physicalExitEvery || since < 0 || !believed && since >= physicalExitRetry) {
			r.asking, r.sent = true, now
			go o.round(r, o.epoch, now)
		}
	}
	o.mu.Unlock()

	for addr := range found {
		addrs = append(addrs, addr.String())
	}
	slices.Sort(addrs)
	return addrs, known
}

// round sends a Round by a reading's interface: its probe URLs asked
// together, each answer taken as it comes.
func (o *physicalExits) round(r *reading, epoch uint64, sent time.Time) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	answered := false
	why := r.noAsker
	for _, url := range r.urls {
		if r.asker == nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), probe.Timeout)
			defer cancel()
			addr, err := r.asker.ExitAddress(ctx, url)
			if err == nil && !(addr.Is4() && sandbox.PublicAddress(addr)) {
				err = fmt.Errorf("%s answered %s, which is not a public IPv4 address", url, addr)
			}
			mu.Lock()
			if err != nil {
				if why == nil {
					why = err
				}
			} else {
				answered = true
			}
			mu.Unlock()
			if err == nil {
				o.answered(r, epoch, sent, addr)
			}
		}()
	}
	wg.Wait()

	o.mu.Lock()
	r.asking = false
	line := ""
	switch {
	case epoch != o.epoch:
		// Forgotten since: there is nothing to say of it.
	case !answered && !r.silent:
		r.silent = true
		line = fmt.Sprintf("this Mac's physical exit address by %s is not read: %v", r.ifc.Name, why)
	case answered && r.silent:
		r.silent = false
		line = fmt.Sprintf("this Mac's physical exit address by %s is read again", r.ifc.Name)
	}
	o.mu.Unlock()
	if line != "" {
		o.say(line)
	}
}

// answered takes one answer to a probe sent at sent. An answer to a
// probe sent before everything was forgotten, or by an interface that
// has changed since, is thrown away.
func (o *physicalExits) answered(r *reading, epoch uint64, sent time.Time, addr netip.Addr) {
	o.mu.Lock()
	if epoch != o.epoch || !o.holdsLocked(r) {
		o.mu.Unlock()
		return
	}
	now := o.now()
	at, had := r.answers[addr]
	if !had || sent.After(at) {
		r.answers[addr] = sent
	}
	// News for whoever asked: an address that was not believed.
	tell := o.changed
	if had {
		if age := now.Sub(at); age >= 0 && age <= physicalExitBelieved {
			tell = nil
		}
	}
	o.mu.Unlock()
	if tell != nil {
		tell()
	}
}

// holdsLocked is whether a reading is still one of the readings.
func (o *physicalExits) holdsLocked(r *reading) bool {
	for _, held := range o.readings {
		if held == r {
			return true
		}
	}
	return false
}

// forget drops everything read: what this Mac's physical exit addresses are
// is not known until they are read again.
func (o *physicalExits) forget() {
	o.mu.Lock()
	o.epoch++
	o.readings = map[string]*reading{}
	o.unreadable = ""
	o.mu.Unlock()
}

// PhysicalExitAddresses returns this Mac's physical exit addresses for a
// sandbox with these gate settings, and whether they are known. beside is
// the interface that is not counted among this Mac's physical Interfaces:
// the sandbox's Interface in use where it expects any exit address but a
// physical one, and none where it expects this Mac's physical exit address.
// A sandbox asks for every heartbeat, and only while its Interface in use
// expects one of the two; nothing is read for a sandbox that does not ask.
func (w *Watchdog) PhysicalExitAddresses(beside string, gate sandbox.GateConfig) (addrs []string, known bool) {
	return w.physical.addresses(beside, gate)
}

// OnPhysicalExitChange has tell called when a physical exit address is
// read that was not believed before, from another goroutine.
func (w *Watchdog) OnPhysicalExitChange(tell func()) {
	w.physical.mu.Lock()
	w.physical.changed = tell
	w.physical.mu.Unlock()
}

// Forget drops everything the Watchdog has read of this Mac: after a
// sleep it may be on another network. Its physical exit addresses are not
// known until they are read again, and the Wi-Fi networks are read
// afresh.
func (w *Watchdog) Forget() {
	w.physical.forget()
	w.mu.Lock()
	w.readAt = time.Time{}
	w.mu.Unlock()
}
