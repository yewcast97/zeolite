// Command gatekeeper runs in the container that owns a sandbox's network
// namespace and enforces its gate: it loads the ruleset, probes the exit
// address and keeps the lease. It carries no packet of the sandbox's and
// listens on nothing.
//
//	gatekeeper run             reads its config and heartbeats on standard input
//	gatekeeper verify <nonce>  checks the live ruleset
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"zeolite/internal/probe"
	"zeolite/internal/sandbox"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "run" {
		if err := run(); err != nil {
			// Written directly: the process is about to exit.
			json.NewEncoder(os.Stdout).Encode(sandbox.GateReport{Error: err.Error()})
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "verify" {
		os.Exit(verify(os.Args[2]))
	}
	fmt.Fprintln(os.Stderr, "usage: gatekeeper run | gatekeeper verify <nonce>")
	os.Exit(2)
}

// statePath is where a running gatekeeper says what `verify` should
// find. It is on a tmpfs of the gatekeeper's container, which the
// workstation does not share.
const statePath = "/run/gatekeeper/state.json"

// tick is how often a running gatekeeper reads the packet counters and
// reports. It judges its gate, and asks the probe URLs whose turn it is,
// at each slot of the probes' pace, which is a tick or a part of one.
const tick = time.Second

// state is what a running gatekeeper loaded.
type state struct {
	RulesetHash string `json:"rulesetHash"`
}

// reporter writes reports to standard output without ever blocking the
// gate on it: Docker's logging can stall, and a gate that waited on it
// would stop probing while its lease was still being renewed.
type reporter chan sandbox.GateReport

func (r reporter) send(rep sandbox.GateReport) {
	select {
	case r <- rep:
	default: // the log is behind: this report is lost, the gate goes on
	}
}

func run() error {
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	line, err := in.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("reading the config: %w", err)
	}
	var cfg sandbox.GatekeeperConfig
	if err := json.Unmarshal(line, &cfg); err != nil {
		return fmt.Errorf("the config is not JSON: %w", err)
	}

	// The ruleset first: the container has no interface but lo until it
	// is in place.
	rules, err := ruleset(cfg)
	if err != nil {
		return err
	}
	kernel := kernelRuleset{hold: cfg.Hold, standIns: answersWithStandIns(cfg)}
	if err := kernel.load(rules); err != nil {
		return err
	}
	hash, err := kernel.hash()
	if err != nil {
		return err
	}

	reports := make(reporter, 256)
	go func() {
		enc := json.NewEncoder(os.Stdout)
		for rep := range reports {
			enc.Encode(rep)
		}
	}()
	failed := func(err error) { fmt.Fprintln(os.Stderr, "gatekeeper:", err) }

	resolver, err := sandbox.ParsePublicIPv4(cfg.Resolver)
	if err != nil {
		return fmt.Errorf("the resolver %q: %w", cfg.Resolver, err)
	}
	// The probe's sockets carry its mark: the ruleset lets them reach
	// the resolver and port 443, and nothing else. They are sent from
	// the Probe Source Ports, which the namespace must not hand out to
	// anything else by itself.
	if err := sourcePortsFree(); err != nil {
		return err
	}
	client, err := probe.New(resolver, cfg.ProbePin, probeControl())
	if err != nil {
		return err
	}
	probes, err := newProber(cfg, client)
	if err != nil {
		return err
	}
	rounds := newPace(len(probes.urls))
	g := newGate(kernel, failed)
	g.hold, g.answerDue, g.butPhysical, g.onlyPhysical = cfg.Hold, rounds.between(), probes.butPhysical(), probes.onlyPhysical()
	g.standIns = probes.standIns
	// A probe is waited for as long as its answer says anything.
	patience := probe.Timeout
	if cfg.Hold {
		patience = probe.HoldTimeout
	}
	// What the gate says is reported at once when it changes: the
	// sandbox's Carrier waits for it. One goroutine writes every such
	// report, each with the gate's state as it is when written, so that
	// none arrives after a later one.
	var passed, rejected atomic.Uint64
	told := make(chan struct{}, 1)
	report := func() {
		select {
		case told <- struct{}{}:
		default:
		}
	}
	// The probes on their way are of the Interface in use: they are
	// ended as soon as the gate takes note of another, or of none.
	var probing underway
	g.changed = func() {
		probing.of(g.inUse())
		report()
	}
	go func() {
		for range told {
			t := g.telemetry()
			// The counters as the last round of probes read them: reading
			// them takes a moment, and this report is waited for.
			t.PacketsPassed, t.PacketsRejected = passed.Load(), rejected.Load()
			reports.send(sandbox.GateReport{Gate: &t})
		}
	}()
	// A lease that nothing renews is over when its time is, and so is a
	// Hold.
	lapse := time.AfterFunc(time.Hour, g.lapse)
	lapse.Stop()
	// Each renewal is reported: the Carrier sends on what the sandbox
	// sends only for as long as the lease it was last told of runs.
	g.renewed = func(ttl time.Duration) { lapse.Reset(ttl); report() }
	release := time.AfterFunc(time.Hour, g.lapse)
	release.Stop()
	g.held = func(ttl time.Duration) { release.Reset(ttl) }

	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(state{RulesetHash: hash})
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		return err
	}

	// Heartbeats are read from here on, so that the lease can be judged
	// as soon as the app sends them.
	go func() {
		dec := json.NewDecoder(in)
		for {
			var h sandbox.Heartbeat
			if err := dec.Decode(&h); err != nil {
				g.heartbeatEnd()
				return
			}
			g.heartbeat(h)
		}
	}()

	reports.send(sandbox.GateReport{Ready: &sandbox.GateReady{RulesetHash: hash}})

	unanswered := &unansweredLog{say: failed, last: map[string]string{}}
	// The timezone of the exit address is read once a probe is answered,
	// and reported in a second ready line: the workstation waits for it,
	// since its TZ is fixed when it starts.
	var timezoneAsked, timezoneKnown atomic.Bool
	timezone := func() {
		defer timezoneAsked.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), probe.Timeout)
		defer cancel()
		iface, _ := g.inUse()
		tz, err := probes.timezone(ctx, cfg.TimezoneProbeURL, iface)
		if err != nil {
			failed(err)
			return
		}
		timezoneKnown.Store(true)
		reports.send(sandbox.GateReport{Ready: &sandbox.GateReady{RulesetHash: hash, Timezone: tz}})
	}
	// The loop turns every slot of the probes' pace: the gate is judged,
	// and the probe URLs whose slot it is are asked, whether or not the
	// probes before were answered: each answer is the gate's as it comes.
	// Every tick the counters are read and a report is written.
	ticker := time.NewTicker(rounds.every())
	defer ticker.Stop()
	var last time.Time
	for {
		g.lapse()
		// The probes are of the Interface in use as it is now. With none
		// there is nothing to probe.
		iface, change := g.inUse()
		of := probing.of(iface, change)
		start := time.Now()
		if iface != "" {
			for _, at := range rounds.due(start, change) {
				url := probes.urls[at]
				go func() {
					ctx, cancel := context.WithTimeout(of, patience)
					defer cancel()
					answer, err := probes.ask(ctx, url, iface)
					// A probe that was ended with its Interface in use
					// was not left unanswered.
					if of.Err() != nil {
						return
					}
					unanswered.note(url, err)
					if err == nil {
						rounds.answer(start)
						g.answered(start, change, answer)
					}
				}()
			}
		}
		if iface != "" && cfg.TimezoneProbeURL != "" && !timezoneKnown.Load() && g.telemetry().ExitAddress != "" && timezoneAsked.CompareAndSwap(false, true) {
			go timezone()
		}
		if start.Sub(last) >= tick-rounds.every()/2 {
			last = start
			if p, r, err := kernel.counters(); err == nil {
				passed.Store(p)
				rejected.Store(r)
			}
			report()
		}
		<-ticker.C
	}
}

// unansweredLog says on standard error, where the container's log keeps
// it, why a probe URL went unanswered: once each time that changes.
type unansweredLog struct {
	say  func(error)
	mu   sync.Mutex
	last map[string]string
}

// note takes what became of one probe of a probe URL: nil when it was
// answered.
func (l *unansweredLog) note(url string, err error) {
	why := ""
	if err != nil {
		why = err.Error()
	}
	l.mu.Lock()
	changed := l.last[url] != why
	l.last[url] = why
	l.mu.Unlock()
	if changed && err != nil {
		l.say(err)
	}
}

// devices lists the network devices of the namespace, by name.
func devices() []string {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// baseChain matches the declaration of a base chain that drops by
// default, as nft lists it.
var baseChain = regexp.MustCompile(`hook (output|input|forward) priority [^;]+; policy drop;`)

// verify checks, in the namespace it runs in and at the moment it runs,
// that the ruleset a gatekeeper loaded is still the live one and that it
// drops by default on every hook, and it lists the devices the namespace
// has. It prints the nonce with its verdict,
// so that whoever asked knows the answer is to this question and not an
// older line.
func verify(nonce string) int {
	var problems []string
	problem := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	var st state
	data, err := os.ReadFile(statePath)
	if err != nil {
		problem("no running gatekeeper recorded its state: %v", err)
	} else if err := json.Unmarshal(data, &st); err != nil {
		problem("the recorded state is unreadable: %v", err)
	}

	kernel := kernelRuleset{}
	listing, err := kernel.listing()
	if err != nil {
		problem("the ruleset is not loaded: %v", err)
	}
	hooks := map[string]bool{}
	for _, m := range baseChain.FindAllStringSubmatch(listing, -1) {
		hooks[m[1]] = true
	}
	for _, hook := range []string{"output", "input", "forward"} {
		if !hooks[hook] {
			problem("no chain drops by default on %s", hook)
		}
	}
	hash, err := kernel.hash()
	if err == nil && st.RulesetHash != "" && hash != st.RulesetHash {
		problem("the live ruleset is not the one the gatekeeper loaded")
	}

	json.NewEncoder(os.Stdout).Encode(struct {
		Nonce       string   `json:"nonce"`
		OK          bool     `json:"ok"`
		RulesetHash string   `json:"rulesetHash"`
		Devices     []string `json:"devices"`
		Problems    []string `json:"problems,omitempty"`
	}{nonce, len(problems) == 0, hash, devices(), problems})
	if len(problems) > 0 {
		return 1
	}
	return 0
}
