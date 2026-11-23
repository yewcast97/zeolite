package main

import (
	"sync"

	"zeolite/internal/sandbox"
)

// awake keeps this Mac from sleeping by itself for as long as a sandbox
// is starting or runs. A Mac that sleeps locks every gate, and a lock
// ends every connection a sandbox has: left alone on battery, this Mac
// would sleep under an agent's download a minute after its user's last
// key. Closing the lid, or choosing Sleep, still puts it to sleep.
type awake struct {
	// keep asks the system to keep this Mac awake while it is idle, and
	// returns what takes the request back. It may wait for the main
	// thread: it is called from run's goroutine alone.
	keep func() (release func())

	mu sync.Mutex
	// live are the sandboxes that are starting or run, by ID.
	live map[string]bool
	// changed is told that live may have another answer than before.
	changed chan struct{}
	// release takes back the request that is held, nil while none is.
	// Only settle reads and writes it.
	release func()
}

func newAwake(keep func() (release func())) *awake {
	return &awake{keep: keep, live: map[string]bool{}, changed: make(chan struct{}, 1)}
}

// note takes an Update: a sandbox's status says whether it has, or is
// about to have, containers. It never waits, whatever goroutine tells.
func (a *awake) note(u sandbox.Update) {
	if u.Status == nil {
		return
	}
	a.mu.Lock()
	switch u.Status.State {
	case sandbox.StateStarting, sandbox.StateGateOpen, sandbox.StateGateLocked:
		a.live[u.SandboxID] = true
	default:
		delete(a.live, u.SandboxID)
	}
	a.mu.Unlock()
	select {
	case a.changed <- struct{}{}:
	default: // one is waiting already, and it reads what is true then
	}
}

// settle asks the system for what the sandboxes need now: this Mac kept
// awake while one is starting or runs, and nothing asked once none is.
func (a *awake) settle() {
	a.mu.Lock()
	wanted := len(a.live) > 0
	a.mu.Unlock()
	switch {
	case wanted && a.release == nil:
		a.release = a.keep()
	case !wanted && a.release != nil:
		a.release()
		a.release = nil
	}
}

// run settles each time a sandbox's status changed, for as long as the
// app runs.
func (a *awake) run() {
	for range a.changed {
		a.settle()
	}
}
