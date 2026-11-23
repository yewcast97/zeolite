package main

import (
	"testing"

	"zeolite/internal/sandbox"
)

// This Mac is kept awake from a sandbox's start until the last sandbox
// has stopped or failed, by one request that is taken back then: a Mac
// that sleeps locks every gate, which ends what the sandboxes download.
func TestTheMacIsKeptAwakeWhileASandboxRuns(t *testing.T) {
	kept, released := 0, 0
	a := newAwake(func() func() {
		kept++
		return func() { released++ }
	})
	say := func(id string, state sandbox.State) {
		t.Helper()
		a.note(sandbox.Update{SandboxID: id, Status: &sandbox.Status{State: state}})
		a.settle()
	}
	held := func(what string, want bool) {
		t.Helper()
		if got := kept-released == 1; got != want || kept-released > 1 || released > kept {
			t.Fatalf("%s: this Mac is kept awake by %d requests, %d taken back", what, kept, released)
		}
	}

	a.settle()
	held("with no sandbox", false)

	// Every state in which a sandbox has, or is about to have, containers
	// keeps this Mac awake, a locked gate too: an agent works there all
	// the same.
	say("s1", sandbox.StateStarting)
	held("a sandbox that is starting", true)
	for _, state := range []sandbox.State{sandbox.StateGateLocked, sandbox.StateGateOpen} {
		say("s1", state)
		held("a sandbox that is "+string(state), true)
	}
	if kept != 1 {
		t.Errorf("one sandbox asked %d times", kept)
	}

	// A second sandbox asks nothing more, and the first one's end takes
	// nothing back while the second runs.
	say("s2", sandbox.StateGateOpen)
	say("s1", sandbox.StateStopped)
	held("one of two sandboxes stopped", true)
	if kept != 1 {
		t.Errorf("two sandboxes asked %d times", kept)
	}

	// News that is no status changes nothing.
	a.note(sandbox.Update{SandboxID: "s2", Gate: &sandbox.GateTelemetry{}})
	a.settle()
	held("a gate's telemetry", true)

	// The last one fails: this Mac may sleep again.
	say("s2", sandbox.StateError)
	held("the last sandbox failed", false)
	say("s2", sandbox.StateStopped)
	held("stopped twice", false)

	// And it is asked again at the next start.
	say("s3", sandbox.StateStarting)
	held("a sandbox started after all had stopped", true)
	if kept != 2 || released != 1 {
		t.Errorf("after a second start: %d requests, %d taken back", kept, released)
	}
}

// Whoever tells of a sandbox is never held up: the system is asked on a
// goroutine of the request's own.
func TestNotingAStatusNeverWaits(t *testing.T) {
	asked := make(chan struct{})
	a := newAwake(func() func() {
		asked <- struct{}{} // the system takes its time
		return func() {}
	})
	go a.run()
	for range 100 {
		a.note(sandbox.Update{SandboxID: "s1", Status: &sandbox.Status{State: sandbox.StateGateOpen}})
	}
	<-asked
}
