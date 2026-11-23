package hostwatch

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// The Interfaces of the tests: a physical one, by a router; a second
// physical one; and what is not physical.
var (
	en0 = physicalInterface{Name: "en0", Index: 11, Addrs: "192.168.1.20", Router: netip.MustParseAddr("192.168.1.1")}
	en5 = physicalInterface{Name: "en5", Index: 14, Addrs: "10.0.0.7", Router: netip.MustParseAddr("10.0.0.1")}
)

// Which Interfaces are physical: up, with an IPv4 address, a
// hardware address and a default route through a router.
func TestWhichInterfacesArePhysical(t *testing.T) {
	addr := func(s ...string) (out []netip.Addr) {
		for _, a := range s {
			out = append(out, netip.MustParseAddr(a))
		}
		return out
	}
	seen := []seenInterface{
		{Name: "lo0", Index: 1, Up: true, Addrs: addr("127.0.0.1")},
		{Name: "en0", Index: 11, Up: true, Hardware: true, Addrs: addr("192.168.1.20")},
		// Up, with a hardware address and an address, and no router: a
		// host-only bridge, as a VM's.
		{Name: "bridge100", Index: 26, Up: true, Hardware: true, Addrs: addr("192.168.64.1")},
		// A tunnel has no hardware address, whatever its routes.
		{Name: "utun9", Index: 30, Up: true, Addrs: addr("10.8.0.2")},
		{Name: "utun10", Index: 31, Up: true, Addrs: addr("10.9.0.2")},
		// A second way out, with two addresses.
		{Name: "en5", Index: 14, Up: true, Hardware: true, Addrs: addr("10.0.0.7", "10.0.0.8")},
		// Down, without an IPv4 address, and without a route.
		{Name: "en6", Index: 15, Hardware: true, Addrs: addr("10.1.0.7")},
		{Name: "en7", Index: 16, Up: true, Hardware: true},
		{Name: "en8", Index: 17, Up: true, Hardware: true, Addrs: addr("169.254.3.4")},
	}
	routers := map[int]netip.Addr{
		11: netip.MustParseAddr("192.168.1.1"),
		14: netip.MustParseAddr("10.0.0.1"),
		15: netip.MustParseAddr("10.1.0.1"),
		16: netip.MustParseAddr("10.2.0.1"),
		// A tunnel whose default route names a router is no more
		// physical for it.
		31: netip.MustParseAddr("10.9.0.1"),
	}
	physical := physicalOf(seen, routers)
	want := []physicalInterface{
		{Name: "en0", Index: 11, Addrs: "192.168.1.20", Router: netip.MustParseAddr("192.168.1.1")},
		{Name: "en5", Index: 14, Addrs: "10.0.0.7 10.0.0.8", Router: netip.MustParseAddr("10.0.0.1")},
	}
	if len(physical) != len(want) {
		t.Fatalf("this Mac's physical Interfaces are %+v, want %+v", physical, want)
	}
	for i := range want {
		if physical[i] != want[i] {
			t.Errorf("physical Interface %d is %+v, want %+v", i, physical[i], want[i])
		}
	}
	if got := physicalOf(seen, nil); len(got) != 0 {
		t.Errorf("with no router in the routing table the physical Interfaces are %+v", got)
	}
}

// mac stands in for this Mac: its physical Interfaces, and what each answers
// to a probe. Rounds are counted as they are sent.
type mac struct {
	t  *testing.T
	mu sync.Mutex
	// physical are its physical Interfaces, and lookErr why they cannot be
	// read.
	physical []physicalInterface
	lookErr  error
	now      time.Time
	// exits are what a probe by an interface is answered, by its name:
	// an address, or nothing for a probe nobody answers.
	exits map[string]string
	// hold, while set, keeps every probe waiting until it is closed.
	hold chan struct{}
	// asked counts the probes sent, by the interface's name and the URL.
	asked map[string]int
	// askers counts the askers made, by the interface's name.
	askers map[string]int
	// changes counts the calls of changed, and said keeps the log.
	changes int
	said    []string
}

type macAsker struct {
	m   *mac
	ifc physicalInterface
}

func (a macAsker) ExitAddress(ctx context.Context, url string) (netip.Addr, error) {
	a.m.mu.Lock()
	a.m.asked[a.ifc.Name+" "+url]++
	exit, hold := a.m.exits[a.ifc.Name], a.m.hold
	a.m.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return netip.Addr{}, ctx.Err()
		}
	}
	if exit == "" {
		return netip.Addr{}, errors.New("no answer")
	}
	return netip.ParseAddr(exit)
}

// gateOf is a gate's settings with the given probe URLs.
func gateOf(urls ...string) sandbox.GateConfig {
	return sandbox.GateConfig{ProbeURLs: urls, Resolver: "1.1.1.1"}
}

var (
	oneURL  = gateOf("https://one.example.com/ip")
	twoURLs = gateOf("https://one.example.com/ip", "https://two.example.com/ip")
)

func newMac(t *testing.T, physical ...physicalInterface) (*mac, *physicalExits) {
	m := &mac{t: t, physical: physical, now: time.Unix(1000, 0), exits: map[string]string{}, asked: map[string]int{}, askers: map[string]int{}}
	o := newPhysicalExits()
	o.look = func() ([]physicalInterface, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return append([]physicalInterface(nil), m.physical...), m.lookErr
	}
	o.newAsker = func(ifc physicalInterface, _ sandbox.GateConfig) (asker, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.askers[ifc.Name]++
		return macAsker{m, ifc}, nil
	}
	o.now = func() time.Time {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.now
	}
	o.say = func(line string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.said = append(m.said, line)
	}
	o.changed = func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.changes++
	}
	return m, o
}

func (m *mac) set(change func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	change()
}

func (m *mac) pass(d time.Duration) { m.set(func() { m.now = m.now.Add(d) }) }

// probes is how many probes were sent by an interface, to any URL.
func (m *mac) probes(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key, count := range m.asked {
		if strings.HasPrefix(key, name+" ") {
			n += count
		}
	}
	return n
}

// settled waits until no Round is under way.
func settled(t *testing.T, o *physicalExits) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		o.mu.Lock()
		asking := false
		for _, r := range o.readings {
			asking = asking || r.asking
		}
		o.mu.Unlock()
		if !asking {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a Round never ended")
		}
	}
}

// read asks for this Mac's physical exit addresses, and waits for the Round
// that sent, if any.
func read(t *testing.T, o *physicalExits, inUse string, gate sandbox.GateConfig) (string, bool) {
	t.Helper()
	addrs, known := o.addresses(inUse, gate)
	settled(t, o)
	return strings.Join(addrs, " "), known
}

func TestThisMacsPhysicalExitAddressesAreRead(t *testing.T) {
	m, o := newMac(t, en0)
	m.exits["en0"] = "203.0.113.7"

	// The first time nothing is known yet, and a Round is sent at once,
	// by the physical Interface, to each probe URL.
	if addrs, known := read(t, o, "utun9", twoURLs); known || addrs != "" {
		t.Fatalf("before any answer: %q, known %v", addrs, known)
	}
	if n := m.probes("en0"); n != 2 {
		t.Fatalf("the first Round sent %d probes by en0, want one to each of two URLs", n)
	}
	// Its answers are told once: the address was not believed before.
	if m.changes != 1 {
		t.Errorf("an address read by two probes of a Round was told %d times", m.changes)
	}
	if addrs, known := read(t, o, "utun9", twoURLs); !known || addrs != "203.0.113.7" {
		t.Fatalf("once answered: %q, known %v", addrs, known)
	}

	// A sandbox asks for every heartbeat; a Round is sent every
	// physicalExitEvery.
	for range 5 {
		m.pass(1500 * time.Millisecond)
		read(t, o, "utun9", twoURLs)
	}
	if n := m.probes("en0"); n != 2 {
		t.Fatalf("within %v of a Round, %d probes had been sent", physicalExitEvery, n)
	}
	m.pass(3 * time.Second)
	if addrs, known := read(t, o, "utun9", twoURLs); !known || addrs != "203.0.113.7" {
		t.Fatalf("at the next Round: %q, known %v", addrs, known)
	}
	if n := m.probes("en0"); n != 4 {
		t.Fatalf("%v after a Round, %d probes had been sent, want a second Round", physicalExitEvery, n)
	}
	// The same address again is no news.
	if m.changes != 1 {
		t.Errorf("an address that was believed already was told: %d times in all", m.changes)
	}

	// Another sandbox with the same settings shares what was read, and
	// one whose Interface in use is another asks the same interface. One
	// with other settings is asked on its own.
	if addrs, known := read(t, o, "utun10", twoURLs); !known || addrs != "203.0.113.7" || m.probes("en0") != 4 {
		t.Errorf("a second sandbox with the same settings: %q, known %v, after %d probes", addrs, known, m.probes("en0"))
	}
	if _, known := read(t, o, "utun9", oneURL); known || m.probes("en0") != 5 {
		t.Errorf("a sandbox with other settings: known %v at once, after %d probes", known, m.probes("en0"))
	}
	if m.askers["en0"] != 2 {
		t.Errorf("%d askers were made for en0, want one for each of two settings", m.askers["en0"])
	}

	// The sandbox leaves by this Mac's physical Interface: there is no other
	// to read, and that is known.
	if addrs, known := read(t, o, "en0", twoURLs); !known || addrs != "" {
		t.Errorf("with the one physical Interface in use: %q, known %v", addrs, known)
	}
	// With no interface left out, as a sandbox asks that expects this Mac's
	// physical exit address, the one physical Interface counts, whichever
	// the sandbox leaves by: what it answered is this Mac's physical exit
	// address, and no probe is sent for it that was not due.
	sent := m.probes("en0")
	if addrs, known := read(t, o, "", twoURLs); !known || addrs != "203.0.113.7" || m.probes("en0") != sent {
		t.Errorf("with no interface left out: %q, known %v, after %d more probes", addrs, known, m.probes("en0")-sent)
	}
}

// Every address a physical Interface answered with in physicalExitBelieved
// is one of this Mac's physical exit addresses, and none that is older.
func TestAnPhysicalExitAddressIsBelievedForAMinute(t *testing.T) {
	m, o := newMac(t, en0)
	m.exits["en0"] = "203.0.113.7"
	read(t, o, "utun9", oneURL)

	// The network leaves from another address at the next Round.
	m.pass(physicalExitEvery)
	m.set(func() { m.exits["en0"] = "203.0.113.8" })
	read(t, o, "utun9", oneURL)
	if addrs, known := read(t, o, "utun9", oneURL); !known || addrs != "203.0.113.7 203.0.113.8" {
		t.Fatalf("after two addresses in two Rounds: %q, known %v", addrs, known)
	}
	if m.changes != 2 {
		t.Errorf("two addresses were told %d times", m.changes)
	}

	// A minute after the probe that saw the first was sent, it is not
	// believed any more; the second is, for ten seconds longer.
	m.pass(physicalExitBelieved - physicalExitEvery)
	if addrs, _ := read(t, o, "utun9", oneURL); addrs != "203.0.113.7 203.0.113.8" {
		t.Errorf("a minute after the first probe, to the second: %q", addrs)
	}
	m.pass(time.Second)
	if addrs, known := read(t, o, "utun9", oneURL); !known || addrs != "203.0.113.8" {
		t.Errorf("past a minute after the first probe: %q, known %v", addrs, known)
	}

	// Nothing answers any more. What was read is believed for its
	// minute, and then this Mac's physical exit addresses are not known.
	m.set(func() { m.exits["en0"] = "" })
	sent := m.probes("en0")
	for range 5 {
		m.pass(physicalExitEvery)
		if _, known := read(t, o, "utun9", oneURL); !known {
			t.Fatal("an answer was not believed for a minute")
		}
	}
	if got := m.probes("en0") - sent; got != 5 {
		t.Errorf("while an answer was believed, %d Rounds went unanswered in %v", got, 5*physicalExitEvery)
	}
	m.pass(physicalExitEvery + time.Second)
	if addrs, known := read(t, o, "utun9", oneURL); known || addrs != "" {
		t.Fatalf("a minute after the last answer: %q, known %v", addrs, known)
	}
	// It is said in the log once that nothing answers, by which
	// interface.
	if len(m.said) != 1 || !strings.Contains(m.said[0], "en0") || !strings.Contains(m.said[0], "no answer") {
		t.Errorf("the log says %q", m.said)
	}

	// While it is not known the interface is asked again sooner.
	sent = m.probes("en0")
	m.pass(physicalExitRetry - time.Millisecond)
	read(t, o, "utun9", oneURL)
	if got := m.probes("en0") - sent; got != 0 {
		t.Errorf("%d Rounds were sent within %v of one that went unanswered", got, physicalExitRetry)
	}
	m.pass(time.Millisecond)
	read(t, o, "utun9", oneURL)
	if got := m.probes("en0") - sent; got != 1 {
		t.Errorf("%d Rounds were sent %v after one that went unanswered, with nothing known", got, physicalExitRetry)
	}
	// It answers again: known again, and both are said once.
	m.set(func() { m.exits["en0"] = "203.0.113.8" })
	m.pass(physicalExitRetry)
	read(t, o, "utun9", oneURL)
	if addrs, known := read(t, o, "utun9", oneURL); !known || addrs != "203.0.113.8" {
		t.Errorf("once it answered again: %q, known %v", addrs, known)
	}
	if len(m.said) != 2 || !strings.Contains(m.said[1], "read again") {
		t.Errorf("the log says %q", m.said)
	}

	// The wall clock is what ages an answer: time the Mac was asleep
	// counts.
	m.pass(8 * time.Hour)
	if addrs, known := o.addresses("utun9", oneURL); known || addrs != nil {
		t.Errorf("after eight hours: %q, known %v", addrs, known)
	}
	settled(t, o)
}

// Known means every physical Interface beside the Interface in use has
// answered: one that has not keeps the others' answers from being all
// there is.
func TestEveryPhysicalInterfaceMustBeRead(t *testing.T) {
	m, o := newMac(t, en0, en5)
	m.exits["en0"] = "203.0.113.7"
	read(t, o, "utun9", oneURL)
	if addrs, known := read(t, o, "utun9", oneURL); known || addrs != "203.0.113.7" {
		t.Fatalf("with en5 unanswered: %q, known %v", addrs, known)
	}
	m.set(func() { m.exits["en5"] = "198.51.100.5" })
	m.pass(physicalExitRetry)
	read(t, o, "utun9", oneURL)
	if addrs, known := read(t, o, "utun9", oneURL); !known || addrs != "198.51.100.5 203.0.113.7" {
		t.Fatalf("with both answered: %q, known %v", addrs, known)
	}
	// The sandbox leaves by en5: en0 alone is beside it.
	if addrs, known := read(t, o, "en5", oneURL); !known || addrs != "203.0.113.7" {
		t.Errorf("with en5 in use: %q, known %v", addrs, known)
	}

	// An answer that is no public IPv4 address is no answer.
	m, o = newMac(t, en0)
	for _, exit := range []string{"192.168.1.20", "2001:db8::7", "100.64.0.9"} {
		m.set(func() { m.exits["en0"] = exit })
		m.pass(physicalExitEvery)
		read(t, o, "utun9", oneURL)
		if addrs, known := read(t, o, "utun9", oneURL); known || addrs != "" {
			t.Errorf("answered %s: %q, known %v", exit, addrs, known)
		}
	}
}

// A Mac with no physical Interface, or whose interfaces cannot be
// read, has no physical exit address that is known.
func TestWithNoPhysicalInterfaceNothingIsKnown(t *testing.T) {
	m, o := newMac(t)
	for range 3 {
		if addrs, known := read(t, o, "utun9", oneURL); known || addrs != "" {
			t.Fatalf("with no physical Interface: %q, known %v", addrs, known)
		}
	}
	if len(m.said) != 1 || !strings.Contains(m.said[0], "no physical Interface") {
		t.Errorf("the log says %q, want it said once", m.said)
	}
	m.set(func() { m.lookErr = errors.New("sysctl: denied") })
	if _, known := read(t, o, "utun9", oneURL); known {
		t.Error("with its interfaces unreadable, this Mac's physical exit addresses are known")
	}
	if len(m.said) != 2 || !strings.Contains(m.said[1], "sysctl: denied") {
		t.Errorf("the log says %q", m.said)
	}
	// A physical Interface appears: read at once.
	m.set(func() { m.lookErr, m.physical, m.exits["en0"] = nil, []physicalInterface{en0}, "203.0.113.7" })
	read(t, o, "utun9", oneURL)
	if addrs, known := read(t, o, "utun9", oneURL); !known || addrs != "203.0.113.7" {
		t.Errorf("once there was one: %q, known %v", addrs, known)
	}
}

// What an interface answered belongs to it as it was. When its index,
// its addresses or its router change, it is not known until it answers
// again, and an answer to a probe sent before is thrown away.
func TestAChangedInterfaceIsReadAgain(t *testing.T) {
	changes := map[string]func(*physicalInterface){
		"its addresses": func(i *physicalInterface) { i.Addrs = "192.168.7.33" },
		"its router":    func(i *physicalInterface) { i.Router = netip.MustParseAddr("192.168.1.254") },
		"its index":     func(i *physicalInterface) { i.Index = 12 },
	}
	for what, change := range changes {
		m, o := newMac(t, en0)
		m.exits["en0"] = "203.0.113.7"
		read(t, o, "utun9", oneURL)
		if _, known := read(t, o, "utun9", oneURL); !known {
			t.Fatal("not known after an answer")
		}

		// A Round is on its way when the interface changes.
		m.pass(physicalExitEvery)
		hold := make(chan struct{})
		m.set(func() { m.hold = hold })
		o.addresses("utun9", oneURL)
		changed := en0
		change(&changed)
		m.set(func() { m.physical, m.exits["en0"] = []physicalInterface{changed}, "198.51.100.77" })
		sent := m.probes("en0")
		if addrs, known := o.addresses("utun9", oneURL); known || len(addrs) != 0 {
			t.Errorf("after %s changed: %q, known %v", what, addrs, known)
		}
		// It is new: a Round is sent at once, though the last was just now.
		for deadline := time.Now().Add(5 * time.Second); m.probes("en0") == sent; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("after %s changed no Round was sent at once", what)
			}
		}
		// The answer to the probe sent before arrives: it says 198.51.100.77
		// too, and is thrown away all the same when its Round's reading is
		// gone. The new Round's is taken.
		m.set(func() { m.hold = nil })
		close(hold)
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			if addrs, known := o.addresses("utun9", oneURL); known && strings.Join(addrs, " ") == "198.51.100.77" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("after %s changed it was never known again", what)
			}
		}
		if m.askers["en0"] != 2 {
			t.Errorf("after %s changed %d askers had been made, want a new one", what, m.askers["en0"])
		}
		settled(t, o)
	}
}

// Forget drops everything: nothing is known until it is read again, and
// an answer to a probe sent before is thrown away.
func TestForget(t *testing.T) {
	m, o := newMac(t, en0)
	w := &Watchdog{readSSIDs: func() []string { return nil }, physical: o}
	m.exits["en0"] = "203.0.113.7"
	read(t, o, "utun9", oneURL)
	if _, known := w.PhysicalExitAddresses("utun9", oneURL); !known {
		t.Fatal("not known after an answer")
	}

	// A Round is on its way, the Mac sleeps, and wakes on another
	// network.
	m.pass(physicalExitEvery)
	hold := make(chan struct{})
	m.set(func() { m.hold = hold })
	w.PhysicalExitAddresses("utun9", oneURL)
	for deadline := time.Now().Add(5 * time.Second); m.probes("en0") < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the Round was never sent")
		}
	}
	w.Forget()
	changes := m.changes
	m.set(func() { m.hold = nil })
	close(hold)
	// Let the answer of before arrive, with nobody asking.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		m.mu.Lock()
		done := m.asked["en0 "+oneURL.ProbeURLs[0]] == 2
		m.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	o.mu.Lock()
	kept := len(o.readings)
	o.mu.Unlock()
	if kept != 0 || m.changes != changes {
		t.Fatalf("after Forget %d readings are kept, and an answer of before was told %d times", kept, m.changes-changes)
	}

	// Asked again: not known, and read anew.
	m.set(func() { m.exits["en0"] = "198.51.100.77" })
	if addrs, known := read(t, o, "utun9", oneURL); known || addrs != "" {
		t.Fatalf("after Forget: %q, known %v", addrs, known)
	}
	if addrs, known := read(t, o, "utun9", oneURL); !known || addrs != "198.51.100.77" {
		t.Errorf("read anew: %q, known %v", addrs, known)
	}
	// The Wi-Fi networks are read afresh too.
	reads := 0
	w.readSSIDs = func() []string { reads++; return []string{"Office"} }
	office := []sandbox.HostCondition{{Kind: sandbox.WiFiSSID, Value: "Office"}}
	w.Met(office)
	w.Forget()
	w.Met(office)
	if reads != 2 {
		t.Errorf("the Wi-Fi networks were read %d times around a Forget, want twice", reads)
	}
}

// Nothing is asked unless a sandbox asks, and what nobody asked for in
// physicalExitBelieved is dropped.
func TestNothingIsReadThatNobodyAsksFor(t *testing.T) {
	m, o := newMac(t, en0)
	m.exits["en0"] = "203.0.113.7"
	if n := m.probes("en0"); n != 0 {
		t.Fatalf("%d probes were sent before any sandbox asked", n)
	}
	read(t, o, "utun9", oneURL)
	read(t, o, "utun9", twoURLs)
	sent := m.probes("en0")
	// Nobody asks for an hour: nothing is sent, by a timer or otherwise.
	time.Sleep(30 * time.Millisecond)
	if n := m.probes("en0"); n != sent {
		t.Fatalf("%d probes were sent with nobody asking", n-sent)
	}

	// One sandbox goes on asking, the other stops.
	for range 7 {
		m.pass(physicalExitEvery)
		read(t, o, "utun9", oneURL)
	}
	o.mu.Lock()
	kept := len(o.readings)
	o.mu.Unlock()
	if kept != 1 {
		t.Errorf("%d readings are kept, want the one that is asked for", kept)
	}
	// The one that stopped starts again: read anew, not believed from
	// before.
	m.set(func() { m.exits["en0"] = "198.51.100.77" })
	if addrs, known := read(t, o, "utun9", twoURLs); known || addrs != "" {
		t.Errorf("asked again after more than a minute: %q, known %v", addrs, known)
	}
}

// One Round at a time by an interface: while one is under way no other
// is sent, however often a sandbox asks.
func TestOneRoundAtATime(t *testing.T) {
	m, o := newMac(t, en0)
	m.exits["en0"] = "203.0.113.7"
	hold := make(chan struct{})
	m.hold = hold
	for range 10 {
		o.addresses("utun9", oneURL)
		o.addresses("utun10", oneURL)
		m.pass(physicalExitEvery)
	}
	for deadline := time.Now().Add(5 * time.Second); m.probes("en0") == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no Round was sent")
		}
	}
	time.Sleep(10 * time.Millisecond)
	if n := m.probes("en0"); n != 1 {
		t.Errorf("%d probes were sent while the first was unanswered", n)
	}
	m.set(func() { m.hold = nil })
	close(hold)
	settled(t, o)
	// Its answer is to a probe sent more than a minute ago by now, and is
	// not believed for that: the next Round's is.
	if addrs, known := read(t, o, "utun9", oneURL); known || addrs != "" {
		t.Errorf("an answer that came after more than a minute: %q, known %v", addrs, known)
	}
	if addrs, known := read(t, o, "utun9", oneURL); !known || addrs != "203.0.113.7" || m.probes("en0") != 2 {
		t.Errorf("once answered: %q, known %v, after %d probes", addrs, known, m.probes("en0"))
	}
}

// On this Mac: its physical Interfaces are read from the real routing table,
// and each is one a sandbox could be configured with. Loopback and a
// tunnel are none.
func TestThisMacsPhysicalInterfaces(t *testing.T) {
	physical, err := lookAtPhysicalInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifc := range physical {
		if ifc.Name == "lo0" || strings.HasPrefix(ifc.Name, "utun") || strings.HasPrefix(ifc.Name, "bridge") {
			t.Errorf("%s is taken for one of this Mac's physical Interfaces: %+v", ifc.Name, ifc)
		}
		if !sandbox.ValidInterfaceName(ifc.Name) || ifc.Index <= 0 || ifc.Addrs == "" || !ifc.Router.Is4() {
			t.Errorf("a physical Interface is read as %+v", ifc)
		}
	}
	t.Logf("this Mac has %d physical Interfaces", len(physical))
}

// A probe bound to loopback reaches nothing: the socket option is on
// the socket, and a public address is not reached another way.
func TestAProbeBoundToLoopbackReachesNothing(t *testing.T) {
	lo := physicalInterface{Name: "lo0", Index: 1}
	a, err := boundAsker(lo, sandbox.GateConfig{Resolver: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	began := time.Now()
	if addr, err := a.ExitAddress(ctx, "https://198.51.100.20/ip"); err == nil {
		t.Fatalf("a probe bound to loopback was answered %v", addr)
	} else if time.Since(began) > 2*time.Second {
		t.Errorf("a probe bound to loopback failed with %v only after %v", err, time.Since(began))
	}
	// And with a resolver that is none, or a pin that is none, no asker
	// is made.
	if _, err := boundAsker(lo, sandbox.GateConfig{Resolver: "192.168.1.1"}); err == nil {
		t.Error("an asker was made with a private resolver")
	}
	if _, err := boundAsker(lo, sandbox.GateConfig{Resolver: "1.1.1.1", ProbePin: "AAAA"}); err == nil {
		t.Error("an asker was made with a pin that is none")
	}
}

// The Host Watchdog opens a connection in one way: through the probe's
// client, on sockets bound to an interface. It has no dialer, listener
// or lookup of its own and no HTTP of its own, sets IP_BOUND_IF in one
// place, and makes a probe's client in one.
func TestTheWatchdogConnectsOnlyThroughBoundProbes(t *testing.T) {
	forbidden := map[string]bool{
		"Dial": true, "DialTimeout": true, "DialTCP": true, "DialUDP": true, "DialIP": true, "DialUnix": true, "Dialer": true,
		"Listen": true, "ListenTCP": true, "ListenUDP": true, "ListenPacket": true, "ListenIP": true, "ListenUnix": true, "ListenConfig": true,
		"FileListener": true, "FilePacketConn": true, "FileConn": true,
		"LookupHost": true, "LookupIP": true, "LookupAddr": true, "LookupCNAME": true, "LookupTXT": true, "LookupSRV": true, "LookupMX": true, "LookupNS": true, "LookupNetIP": true, "LookupIPAddr": true, "LookupPort": true,
		"Resolver": true, "DefaultResolver": true,
	}
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files, bound, clients, selectors := 0, 0, 0, 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for _, imp := range file.Imports {
			switch path := strings.Trim(imp.Path.Value, `"`); path {
			case "net/http", "net/rpc", "net/smtp", "crypto/tls", "golang.org/x/net/proxy", "golang.org/x/net/websocket":
				t.Errorf("%s imports %s", name, path)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			selectors++
			pkg, _ := sel.X.(*ast.Ident)
			switch {
			case pkg != nil && pkg.Name == "net" && forbidden[sel.Sel.Name]:
				t.Errorf("%s uses net.%s: the Host Watchdog connects through the probe's client alone", name, sel.Sel.Name)
			case sel.Sel.Name == "Accept" || strings.HasPrefix(sel.Sel.Name, "Serve"):
				t.Errorf("%s uses %s: the Host Watchdog listens on nothing", name, sel.Sel.Name)
			case sel.Sel.Name == "IP_BOUND_IF":
				bound++
				if name != "bound_darwin.go" {
					t.Errorf("%s sets IP_BOUND_IF: only bound_darwin.go does", name)
				}
			case pkg != nil && pkg.Name == "probe" && sel.Sel.Name == "New":
				clients++
				if name != "physicalexit.go" {
					t.Errorf("%s makes a probe's client: only boundAsker, in physicalexit.go, does", name)
				}
			}
			return true
		})
	}
	if files < 4 || selectors < 150 {
		t.Fatalf("read %d files with %d names: the test would prove little", files, selectors)
	}
	if bound != 1 || clients != 1 {
		t.Errorf("the Host Watchdog binds in %d places and makes a probe's client in %d: want one of each", bound, clients)
	}
}
