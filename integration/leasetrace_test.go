//go:build integration

package integration

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// leaseTraceEnv names how long the lease trace runs: "10m". Without it
// the trace is skipped: it takes that long, and proves nothing by
// passing.
const leaseTraceEnv = "ZEOLITE_LEASE_TRACE"

// answeredProbe is one probe that was answered with an expected address
// in time to renew a lease: when it was sent, and when its answer came.
type answeredProbe struct {
	sent, answered time.Time
}

// The lease trace says what a shorter lease would cost on the ways out
// this Mac has: how often it would run out for want of an answer in time.
// It runs a sandbox that keeps no Hold, whose lease is the longer one, by
// this Mac's physical Interface, and one by the virtual Interface where
// there is one, and takes from each word of the gate when the probe was
// sent that last renewed the lease and when its answer came. Against those
// it counts, for each duration a lease might have, how often and for how
// long such a lease would have been over.
//
// The probes are sent at the pace they are sent at whatever the lease
// lasts, so one trace speaks for every duration no longer than the
// lease it was taken under.
func TestLeaseTrace(t *testing.T) {
	length, err := time.ParseDuration(os.Getenv(leaseTraceEnv))
	if err != nil || length <= 0 {
		t.Skipf("set %s to how long the trace runs, as in %s=10m", leaseTraceEnv, leaseTraceEnv)
	}
	ways := map[string]sandbox.Config{"this Mac's physical Interface": openConfig(t, "lease-trace-physical")}
	if newVirtualInterface != nil {
		ways["the virtual Interface"] = virtualInterfaceConfig(newVirtualInterface(t), "lease-trace-virtual")
	}
	var mu sync.Mutex
	traces := map[string][]answeredProbe{}
	// Beside each, a sandbox that keeps a Hold, by the same way out:
	// how often its lease, which is the shorter, did run out.
	type lapses struct {
		times         int
		since         time.Time
		over, longest time.Duration
	}
	held := map[string]*lapses{}
	for way, cfg := range ways {
		kept := cfg
		kept.ID, kept.Name, kept.Gate.Hold = cfg.ID+"-hold", cfg.Name+" with a Hold", true
		ran := &lapses{}
		held[way] = ran
		updates.tapGate(kept.ID, func(at time.Time, gate sandbox.GateTelemetry) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case !gate.Open && ran.since.IsZero():
				ran.since = at
			case gate.Open && !ran.since.IsZero():
				ran.times++
				ran.over += at.Sub(ran.since)
				ran.longest = max(ran.longest, at.Sub(ran.since))
				ran.since = time.Time{}
			}
		})
		start(t, kept, nil, nil)
		var until time.Time
		updates.tapGate(cfg.ID, func(at time.Time, gate sandbox.GateTelemetry) {
			if !gate.Open || gate.LeaseFor <= 0 {
				return
			}
			// A lease that runs until later than the last word said was
			// renewed: by a probe sent a lease's duration before that.
			if runs := at.Add(gate.LeaseFor); runs.Sub(until) > 20*time.Millisecond {
				until = runs
				mu.Lock()
				traces[way] = append(traces[way], answeredProbe{sent: runs.Add(-sandbox.LeaseDuration), answered: at})
				mu.Unlock()
			}
		})
		start(t, cfg, nil, nil)
	}
	for _, cfg := range ways {
		waitState(t, cfg.ID, sandbox.StateGateOpen, 60*time.Second)
		waitState(t, cfg.ID+"-hold", sandbox.StateGateOpen, 60*time.Second)
	}
	began := time.Now()
	mu.Lock()
	for _, ran := range held {
		*ran = lapses{}
	}
	mu.Unlock()
	time.Sleep(length)
	ended := time.Now()

	mu.Lock()
	defer mu.Unlock()
	for way, ran := range held {
		t.Logf("%s, with a Hold, %v: the lease ran out %d times, %.1f an hour, for %v in all and %v at most",
			way, ended.Sub(began).Round(time.Second), ran.times, float64(ran.times)/ended.Sub(began).Hours(), ran.over.Round(10*time.Millisecond), ran.longest.Round(10*time.Millisecond))
	}
	for way, trace := range traces {
		// What came before the gate first opened is the start, not the
		// way out.
		var probes []answeredProbe
		var took []time.Duration
		for _, p := range trace {
			if p.answered.After(began) {
				probes = append(probes, p)
				took = append(took, p.answered.Sub(p.sent))
			}
		}
		if len(probes) < 10 {
			t.Errorf("%s: %d probes renewed the lease in %v", way, len(probes), length)
			continue
		}
		sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
		at := func(share float64) time.Duration {
			return took[int(share*float64(len(took)-1))].Round(10 * time.Millisecond)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s, %v: %d probes renewed the lease, one every %v; an answer took %v at the median, %v at 90%%, %v at 99%%, %v at most\n",
			way, ended.Sub(began).Round(time.Second), len(probes), (ended.Sub(began) / time.Duration(len(probes))).Round(10*time.Millisecond), at(0.5), at(0.9), at(0.99), at(1))
		for _, lease := range []time.Duration{time.Second, 1500 * time.Millisecond, 2 * time.Second, 2500 * time.Millisecond, 3 * time.Second, 4 * time.Second, sandbox.LeaseDuration} {
			over, longest, times := wouldRunOut(probes, ended, func(p answeredProbe) time.Time { return p.sent.Add(lease) })
			fmt.Fprintf(&b, "  a lease of %-4v from when the probe was sent would have run out %3d times, %5.1f an hour, for %v in all and %v at most\n",
				lease, times, float64(times)/ended.Sub(began).Hours(), over.Round(10*time.Millisecond), longest.Round(10*time.Millisecond))
		}
		// The one a gate that keeps a Hold has with three probe URLs is
		// among them: the time two answers are due in, and its margin.
		for _, after := range []time.Duration{500 * time.Millisecond, 750 * time.Millisecond, time.Second, 1500 * time.Millisecond, 2 * time.Second, 2*(2*time.Second/3) + sandbox.HoldLeaseMargin, 2500 * time.Millisecond} {
			var lasted time.Duration
			over, longest, times := wouldRunOut(probes, ended, func(p answeredProbe) time.Time {
				runs := p.answered.Add(after)
				if latest := p.sent.Add(sandbox.LeaseDuration); latest.Before(runs) {
					runs = latest
				}
				lasted += runs.Sub(p.sent)
				return runs
			})
			fmt.Fprintf(&b, "  a lease until %-5v after the answer came would have run out %3d times, %5.1f an hour, for %v in all and %v at most; it lasts %v from when the probe was sent, on average\n",
				after.Round(10*time.Millisecond), times, float64(times)/ended.Sub(began).Hours(), over.Round(10*time.Millisecond), longest.Round(10*time.Millisecond), (lasted / time.Duration(len(probes))).Round(10*time.Millisecond))
		}
		t.Log(b.String())
	}
}

// wouldRunOut counts how often a lease would have been over between the
// first of some answered probes and an end, for how long in all, and
// for how long at most. runs says until when an answer renews the
// lease: it does if it came before that.
func wouldRunOut(probes []answeredProbe, end time.Time, runs func(answeredProbe) time.Time) (over, longest time.Duration, times int) {
	sort.Slice(probes, func(i, j int) bool { return probes[i].answered.Before(probes[j].answered) })
	var until time.Time
	out := func(from, to time.Time) {
		if !until.IsZero() && to.After(from) {
			times++
			over += to.Sub(from)
			longest = max(longest, to.Sub(from))
		}
	}
	for _, p := range probes {
		renewed := runs(p)
		if !p.answered.Before(renewed) {
			continue
		}
		out(until, p.answered)
		if renewed.After(until) {
			until = renewed
		}
	}
	// A lease no answer came in time for was never there to run out:
	// it is over from the first to the last.
	if until.IsZero() && len(probes) > 0 {
		return end.Sub(probes[0].answered), end.Sub(probes[0].answered), 1
	}
	out(until, end)
	return over, longest, times
}
