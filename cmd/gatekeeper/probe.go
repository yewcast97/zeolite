package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sync"
	"time"

	"zeolite/internal/probe"
	"zeolite/internal/sandbox"
)

const (
	// probeInterval is how often each probe URL is asked, and
	// unansweredInterval how long a probe may go unanswered before each
	// is asked twice as often: a way out that answers at once is asked
	// half as often as one that is slow or silent, where the next answer
	// is the more needed.
	probeInterval      = 2 * time.Second
	unansweredInterval = time.Second
	// maxSpread is over how many slots, at most, half a round is spread:
	// with more probe URLs than that, some are asked together.
	maxSpread = 10
)

// pace says which probe URLs are asked at each of its slots. The first
// round of an Interface in use is sent together: what the gate first
// hears of it, it hears of every probe URL at once. After that a round
// is spread over probeInterval, one probe URL after another at even
// distances, so that a change of where the Interface leads is asked
// about soon, not a whole interval later; and the answers that renew the
// lease come as evenly.
//
// A slot is unansweredInterval divided by the number of probe URLs, and
// twice that number of slots are a round's time. Each probe URL has one
// slot of them where it is asked, and a second, unansweredInterval
// later, where it is asked again while the answers are late: while a
// probe that is unansweredInterval old has no answer, and none sent
// since has one.
type pace struct {
	mu sync.Mutex
	// urls is how many probe URLs there are, and spread over how many
	// slots half a round is spread.
	urls, spread int
	// started is whether a round was sent, change the count of changes
	// of the Interface in use it was a round of, and slot the slot since
	// the first round of that Interface in use.
	started bool
	change  uint64
	slot    int
	// waiting are when the probes were sent that are not older than one
	// that was answered, the oldest first: as many as it takes to say
	// that the answers are late.
	waiting []time.Time
}

// newPace makes the pace of so many probe URLs.
func newPace(urls int) *pace {
	return &pace{urls: urls, spread: min(urls, maxSpread)}
}

// every is how long a slot lasts: how often due is asked.
func (p *pace) every() time.Duration {
	return unansweredInterval / time.Duration(p.spread)
}

// between is how long lies between one probe and the next while the
// answers are in time: a round's time, spread over the probe URLs.
func (p *pace) between() time.Duration {
	return 2 * p.every()
}

// due is asked once a slot, and returns which probe URLs are asked now,
// by their place in the config, of the Interface in use whose count of
// changes is change. What was asked of the Interface in use before a
// change says nothing of the one in use now: its first round is sent at
// once, together, as the very first is.
func (p *pace) due(now time.Time, change uint64) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ask []int
	if !p.started || change != p.change {
		p.started, p.change, p.slot, p.waiting = true, change, 0, nil
		for url := range p.urls {
			ask = append(ask, url)
		}
	} else {
		p.slot = (p.slot + 1) % (2 * p.spread)
		// A slot comes a moment early or late: half of one does not
		// count.
		late := len(p.waiting) > 0 && now.Sub(p.waiting[0]) >= unansweredInterval-p.every()/2
		for url := range p.urls {
			first := 2 * url % (2 * p.spread)
			if p.slot == first || late && p.slot == (first+p.spread)%(2*p.spread) {
				ask = append(ask, url)
			}
		}
	}
	// No more wait than can be on their way at once, at the faster pace
	// and for as long as a probe is waited for: past that nothing has
	// answered for so long that the oldest say all there is to say.
	for range ask {
		if len(p.waiting) < p.urls*(1+int(probe.HoldTimeout/unansweredInterval)) {
			p.waiting = append(p.waiting, now)
		}
	}
	return ask
}

// answer takes note of an answer, with whatever address, to a probe
// that was sent at start: no probe sent before it, or with it, is waited
// for any more.
func (p *pace) answer(start time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.waiting) > 0 && !p.waiting[0].After(start) {
		p.waiting = p.waiting[1:]
	}
}

// underway are the probes that are on their way: all of one Interface
// in use. When another is in use, or none, they are ended at once: what
// they would answer says nothing of it, and a connection that is still
// being made would go on asking for an Interface that is not there.
type underway struct {
	mu     sync.Mutex
	iface  string
	change uint64
	ctx    context.Context
	end    context.CancelFunc
}

// of returns the context of the probes of an Interface in use, by its
// name and the count of its changes, and ends those of any other.
func (u *underway) of(iface string, change uint64) context.Context {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ctx == nil || iface != u.iface || change != u.change {
		if u.end != nil {
			u.end()
		}
		u.iface, u.change = iface, change
		u.ctx, u.end = context.WithCancel(context.Background())
	}
	return u.ctx
}

// probeAnswer is what one probe URL answered.
type probeAnswer struct {
	// exit is the address it answered.
	exit string
	// matches is whether that address is expected at the Interface in
	// use, as far as the config says: where this Mac's physical exit
	// address is expected, or any but it, the gate holds it against
	// those besides.
	matches bool
	latency time.Duration
}

// expectation is what the probe may see while the sandbox leaves by one
// Interface: one of some addresses, or any address, or any IPv4 address
// but one of this Mac's physical exit addresses, or one of those and no
// other, as its owner said. Which those are the gate hears from the
// app, and judges by.
type expectation struct {
	any          bool
	butPhysical  bool
	onlyPhysical bool
	addrs        map[netip.Addr]bool
}

// asker sends a probe: what a probe.Client does.
type asker interface {
	// ExitAddress asks a probe URL what address it was called from.
	ExitAddress(ctx context.Context, url string) (netip.Addr, error)
	// Fetch returns what a URL answers, trimmed.
	Fetch(ctx context.Context, url string) (string, error)
}

// prober reads the exit address: it asks a probe URL what address it
// was called from, and judges the answer.
type prober struct {
	urls []string
	// expected is what the probe may see, by the Interface in use.
	expected map[string]expectation
	// standIns names the Interfaces where names are answered with
	// stand-in addresses: a probe sent by one of them connects to the
	// stand-in address its probe URL's name has there.
	standIns map[string]bool
	probes   asker
}

// newProber makes a prober whose probes the asker sends.
func newProber(cfg sandbox.GatekeeperConfig, probes asker) (*prober, error) {
	p := &prober{urls: cfg.ProbeURLs, expected: map[string]expectation{}, standIns: map[string]bool{}, probes: probes}
	if len(p.urls) == 0 {
		return nil, errors.New("no probe URL")
	}
	if len(cfg.AllowedInterfaces) == 0 {
		return nil, errors.New("no Allowed Interface")
	}
	for _, ifc := range cfg.AllowedInterfaces {
		if _, twice := p.expected[ifc.Name]; twice || !sandbox.ValidInterfaceName(ifc.Name) {
			return nil, fmt.Errorf("%q is not an Interface, or is listed twice", ifc.Name)
		}
		e := expectation{any: ifc.AnyExitAddress, butPhysical: ifc.AnyExitAddressButPhysical, onlyPhysical: ifc.PhysicalExitAddress, addrs: map[netip.Addr]bool{}}
		for _, s := range ifc.ExpectedExitAddresses {
			a, err := sandbox.ParsePublicIPv4(s)
			if err != nil {
				return nil, fmt.Errorf("%s: expected exit address %q: %w", ifc.Name, s, err)
			}
			e.addrs[a] = true
		}
		// Each is taken only where it is said, and never beside another.
		if ifc.Expectations() != 1 {
			return nil, fmt.Errorf("%s: it has expected exit addresses, or any, or any but physical, or physical: one of the four", ifc.Name)
		}
		p.expected[ifc.Name] = e
		if ifc.StandInAddresses {
			p.standIns[ifc.Name] = true
		}
	}
	return p, nil
}

// by returns the context of a probe that is sent by an Interface in
// use: one that reaches stand-in addresses where names are answered
// with them there.
func (p *prober) by(ctx context.Context, iface string) context.Context {
	if p.standIns[iface] {
		return probe.ReachingStandIns(ctx)
	}
	return ctx
}

// ask asks one probe URL, on a connection of its own, and judges its
// answer by what is expected at the Interface in use. An error is a
// probe nobody answered.
func (p *prober) ask(ctx context.Context, url, iface string) (probeAnswer, error) {
	start := time.Now()
	addr, err := p.probes.ExitAddress(p.by(ctx, iface), url)
	if err != nil {
		return probeAnswer{}, fmt.Errorf("the probe of %s was not answered: %w", url, err)
	}
	// An Interface the config does not name expects nothing.
	expected := p.expected[iface]
	return probeAnswer{
		exit: addr.String(),
		// This Mac's physical exit addresses are read over IPv4, as the
		// sandbox leaves over IPv4 alone: an answer of another family
		// cannot be held against them, and is not expected.
		matches: expected.any || expected.addrs[addr] || (expected.butPhysical || expected.onlyPhysical) && addr.Is4(),
		latency: time.Since(start),
	}, nil
}

// butPhysical names the Interfaces that expect any exit address but a
// physical one, and onlyPhysical those that expect this Mac's physical
// exit address.
func (p *prober) butPhysical() map[string]bool {
	names := map[string]bool{}
	for name, e := range p.expected {
		if e.butPhysical {
			names[name] = true
		}
	}
	return names
}

func (p *prober) onlyPhysical() map[string]bool {
	names := map[string]bool{}
	for name, e := range p.expected {
		if e.onlyPhysical {
			names[name] = true
		}
	}
	return names
}

var timezonePattern = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)

// timezone reads the IANA timezone name of the exit address, by the
// Interface in use. The app checks that the name is one it knows; here
// it only has to be shaped like one.
func (p *prober) timezone(ctx context.Context, url, iface string) (string, error) {
	tz, err := p.probes.Fetch(p.by(ctx, iface), url)
	if err != nil {
		return "", err
	}
	if !timezonePattern.MatchString(tz) {
		return "", fmt.Errorf("the timezone endpoint answered %q", tz)
	}
	return tz, nil
}
