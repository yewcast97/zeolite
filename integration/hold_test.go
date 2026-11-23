//go:build integration

package integration

import (
	"bufio"
	"io"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// quietStream is a sandbox's Packet stream as its Carrier reads it,
// with a switch: while it is quiet, the sandbox's packets reach the
// Carrier only where they are for one address. The probes go unanswered
// then, and a connection to that address goes on, as on a way out whose
// probes are slow.
type quietStream struct {
	io.ReadWriteCloser
	quiet atomic.Bool

	mu sync.Mutex
	// but is the one address whose packets cross while it is quiet.
	but     netip.Addr
	frames  *bufio.Reader
	waiting []byte
}

func (s *quietStream) Read(p []byte) (int, error) {
	packet := make([]byte, sandbox.MaxPacket)
	for len(s.waiting) == 0 {
		n, err := sandbox.ReadPacket(s.frames, packet)
		if err != nil {
			return 0, err
		}
		s.mu.Lock()
		but := s.but
		s.mu.Unlock()
		if to, ok := netip.AddrFromSlice(packet[16:20]); n >= 20 && s.quiet.Load() && (!ok || to != but) {
			continue
		}
		if s.waiting, err = sandbox.AppendPacket(nil, packet[:n]); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.waiting)
	s.waiting = s.waiting[n:]
	return n, nil
}

// startHeld starts a sandbox that keeps a Hold, with a download under
// way, and returns the switch that silences its probes, when the
// download ends, and its server.
func startHeld(t *testing.T, id string, conditionsMet func() bool) (*quietStream, <-chan time.Time, netip.Addr) {
	t.Helper()
	cfg := openConfig(t, id)
	cfg.Gate.Hold = true
	var stream *quietStream
	real := carry(t, cfg.ID)
	startWith(t, cfg, nil, orchestrator.StartOptions{
		ConditionsMet: conditionsMet,
		Carry: func(c sandbox.CarrierConfig, packets io.ReadWriteCloser, report func(sandbox.CarrierReport), ended func(error)) (orchestrator.Carrier, error) {
			stream = &quietStream{ReadWriteCloser: packets, frames: bufio.NewReaderSize(packets, 1<<16)}
			return real(c, stream, report, ended)
		},
	})
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	done, server := download(t, cfg.ID)
	stream.mu.Lock()
	stream.but = server
	stream.mu.Unlock()
	return stream, done, server
}

// A sandbox that keeps a Hold, on a way out whose probes stop being
// answered with a download under way. The gate locks when the lease is
// over, and the download goes on: what its server sends still arrives,
// and the sandbox still acknowledges it. Nothing new is made: a
// connection is refused at once, and no name is resolved. When the Hold
// is over the download is cut, a Hold's duration after the last probe
// that was answered, and nothing leaves any more. With the probes
// answered again, the gate opens.
func TestAHoldKeepsADownloadWhileNoProbeAnswers(t *testing.T) {
	const id = "hold-kept"
	stream, done, server := startHeld(t, id, nil)
	capture := captureTUN(t, id)

	stream.quiet.Store(true)
	silenced := time.Now()
	waitFor(t, sandbox.LeaseDuration+3*time.Second, "the gate to lock with its Hold lasting", func() bool {
		gate := updates.gateOf(id)
		return !gate.Open && gate.Held
	})
	if gate := updates.gateOf(id); gate.LockReason != sandbox.LockProbeUnanswered {
		t.Errorf("the gate that holds is locked for %q, want %q", gate.LockReason, sandbox.LockProbeUnanswered)
	}
	if status := updates.statusOf(id); status.State != sandbox.StateGateLocked {
		t.Errorf("the sandbox whose gate holds is %s", status)
	}

	// The download goes on under the Hold.
	before := capture.sentTo(t, server)
	time.Sleep(time.Second)
	if after := capture.sentTo(t, server); after <= before {
		t.Errorf("under the Hold the sandbox sent %d packets of its download in a second", after-before)
	}
	select {
	case ended := <-done:
		t.Fatalf("the download ended %v after the probes were silenced, under a Hold", ended.Sub(silenced).Round(100*time.Millisecond))
	default:
	}
	// Nothing new is made: the kernel refuses the connection itself.
	began := time.Now()
	if got := attempt(t, id, "1.1.1.1", 80); got != refused {
		t.Errorf("under the Hold a new connection to a public address was %s, want %s", got, refused)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the refusal of a new connection under the Hold took %v", took)
	}
	if out, _ := inSandbox(id, resolve+"example.com "+testResolver.String()); trimmed(out) != "no answer" {
		t.Errorf("under the Hold a name query was answered: %q", trimmed(out))
	}
	// A connection to the download's own server too, which the test
	// lets across: it is new, and the Hold keeps what was made.
	if got := attempt(t, id, server.String(), 443); got != refused {
		t.Errorf("under the Hold a new connection to the download's server was %s, want %s", got, refused)
	}

	// The Hold ends its duration after the last probe that was answered,
	// which was sent at most a Round before the probes were silenced.
	select {
	case ended := <-done:
		lasted := ended.Sub(silenced)
		if lasted < sandbox.HoldDuration-3*time.Second || lasted > sandbox.HoldDuration+2*time.Second {
			t.Errorf("the download went on for %v after the probes were silenced, want about %v", lasted.Round(100*time.Millisecond), sandbox.HoldDuration)
		}
	case <-time.After(sandbox.HoldDuration + 15*time.Second):
		t.Fatal("the download outlived the Hold")
	}
	waitFor(t, 3*time.Second, "the gate to say that it holds nothing", func() bool {
		gate := updates.gateOf(id)
		return !gate.Open && !gate.Held
	})
	expectStopped(t, capture, server, "the Hold was over")
	if got := attempt(t, id, "1.1.1.1", 80); got != refused {
		t.Errorf("after the Hold a connection to a public address was %s, want %s", got, refused)
	}

	// Answered again: open.
	stream.quiet.Store(false)
	waitState(t, id, sandbox.StateGateOpen, 15*time.Second)
	if got := attempt(t, id, "1.1.1.1", 80); got != answered {
		t.Errorf("with the gate open again a public address was %s, want %s", got, answered)
	}
	capture.expectGated(t)
}

// Under a Hold, what locks the gate for a reason it knows cuts every
// connection at once: a Host Condition that stops holding ends the
// download long before the Hold would have.
func TestAHoldEndsAtOnceForWhatTheGateKnows(t *testing.T) {
	const id = "hold-ended"
	var unmet atomic.Bool
	stream, done, server := startHeld(t, id, func() bool { return !unmet.Load() })
	capture := captureTUN(t, id)

	stream.quiet.Store(true)
	waitFor(t, sandbox.LeaseDuration+3*time.Second, "the gate to lock with its Hold lasting", func() bool {
		gate := updates.gateOf(id)
		return !gate.Open && gate.Held
	})
	unmet.Store(true)
	since := time.Now()
	waitLockedFor(t, id, sandbox.LockHostCondition, 4*time.Second)
	if updates.gateOf(id).Held {
		t.Error("a gate that is locked for a Host Condition says that it holds")
	}
	expectCut(t, done, since, 3*time.Second, "a Host Condition that stopped holding under a Hold")
	expectStopped(t, capture, server, "the Hold was ended")
	capture.expectGated(t)
}

// keptScript makes a connection to a server that keeps one, and asks
// once. When the file /tmp/ask is there it asks again, on the same
// connection, and says whether an answer came within three seconds, and
// if none did, whether one came later.
const keptScript = `
import os, socket, sys, time
addr = sys.argv[1]
ask = ("HEAD / HTTP/1.1\r\nHost: %s\r\n\r\n" % addr).encode()
s = socket.create_connection((addr, 80), 8)
s.settimeout(8)
s.sendall(ask)
print("first", "answered" if s.recv(4096) else "closed", flush=True)
while not os.path.exists("/tmp/ask"):
    time.sleep(0.05)
s.sendall(ask)
s.settimeout(3)
try:
    print("held", "answered" if s.recv(4096) else "closed", flush=True)
except socket.timeout:
    print("held", "nothing", flush=True)
    s.settimeout(40)
    try:
        print("then", "answered" if s.recv(4096) else "closed", flush=True)
    except OSError as e:
        print("then", e, flush=True)
except OSError as e:
    print("held", e, flush=True)
`

// Under a Hold, what the sandbox sends on a connection it has does not
// leave this Mac: a second request on a connection that was answered
// before gets no answer while the Hold lasts, though its packets cross
// to the Carrier, and is answered once the gate is open again, on the
// same connection. The gate opens from under the Hold: the probe's
// connections are made and carried while the sandbox's own wait.
func TestUnderAHoldWhatTheSandboxSendsWaits(t *testing.T) {
	const id = "hold-waits"
	stream, _, _ := startHeld(t, id, nil)
	// The one address whose packets cross while the probes are silenced
	// is the kept connection's.
	kept := netip.MustParseAddr("1.1.1.1")
	stream.mu.Lock()
	stream.but = kept
	stream.mu.Unlock()
	inSandbox(id, "rm -f /tmp/ask")
	said := make(chan string, 1)
	go func() {
		out, _ := inSandbox(id, "python3 -c "+shellQuote(keptScript)+" "+kept.String())
		said <- out
	}()
	time.Sleep(2 * time.Second)

	stream.quiet.Store(true)
	waitFor(t, sandbox.LeaseDuration+3*time.Second, "the gate to lock with its Hold lasting", func() bool {
		gate := updates.gateOf(id)
		return !gate.Open && gate.Held
	})
	inSandbox(id, "touch /tmp/ask")
	time.Sleep(4 * time.Second)
	if gate := updates.gateOf(id); gate.Open || !gate.Held {
		t.Fatalf("four seconds into the Hold the gate says %+v", gate)
	}

	stream.quiet.Store(false)
	waitState(t, id, sandbox.StateGateOpen, 15*time.Second)
	select {
	case out := <-said:
		if got := strings.Join(strings.Fields(out), " "); got != "first answered held nothing then answered" {
			t.Errorf("the connection the sandbox kept says %q, want the first request answered, the second not under the Hold, and then answered", got)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("the connection the sandbox kept said nothing")
	}
}
