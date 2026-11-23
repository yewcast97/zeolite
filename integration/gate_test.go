//go:build integration

package integration

import (
	"net"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// leakScript tries to get a packet out of a sandbox by every route that
// no gate passes, open or locked, and prints one line per attempt.
const leakScript = `
import socket, struct
def attempt(name, fn):
    try: print(name, fn())
    except OSError as e: print(name, "refused")
def udp():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.sendto(b"x", ("1.1.1.1", 9999)); return "SENT"
def udp_to_another_resolver():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.sendto(b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01", ("8.8.8.8", 53)); return "SENT"
def quic_to_443():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.sendto(b"\xc0\x00\x00\x00\x01" + b"\x00" * 1195, ("1.1.1.1", 443)); return "SENT"
def udp_bound_to_lo():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, 25, b"lo\0"); s.sendto(b"x", ("1.1.1.1", 9999)); return "SENT"
def ping():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_ICMP)
    s.sendto(b"\x08\x00\xf7\xff\x00\x00\x00\x00", ("1.1.1.1", 0)); return "SENT"
def raw_socket():
    socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP); return "OPENED"
def packet_socket():
    socket.socket(socket.AF_PACKET, socket.SOCK_RAW, 0x0300); return "OPENED"
def multicast():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, struct.pack("4s4s", socket.inet_aton("224.0.0.251"), socket.inet_aton("0.0.0.0")))
    s.sendto(b"x", ("224.0.0.251", 5353)); return "SENT"
def socket_mark():
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, 36, 1); return "MARKED"
def ipv6():
    s = socket.socket(socket.AF_INET6); s.settimeout(2); s.connect(("2606:4700:4700::1111", 80)); return "CONNECTED"
for n, f in list(globals().items()):
    if callable(f) and f.__module__ == "__main__" and n != "attempt": attempt(n, f)
`

// resolve is a script that prints the A record a resolver gives a name.
const resolve = `python3 -c '
import socket, struct, sys
name, server = sys.argv[1], sys.argv[2]
q = b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00" + b"".join(bytes([len(p)]) + p.encode() for p in name.split(".")) + b"\x00\x00\x01\x00\x01"
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
try:
    s.sendto(q, (server, 53)); r = s.recv(512)
except OSError as e:
    print("no answer"); sys.exit()
rcode, answers = r[3] & 15, struct.unpack("!H", r[6:8])[0]
print("rcode=%d" % rcode, socket.inet_ntoa(r[-4:]) if answers else "no data")
' `

// leakAttempts is how many routes leakScript tries.
const leakAttempts = 10

// expectNoLeak runs the leak attempts in a sandbox and fails the test
// for each that was not refused. What actually left is for the TUN
// capture to say.
func expectNoLeak(t *testing.T, sandboxID, gate string) {
	t.Helper()
	out, _ := inSandbox(sandboxID, "python3 -c "+shellQuote(leakScript))
	lines := strings.Split(trimmed(out), "\n")
	for _, line := range lines {
		if !strings.HasSuffix(line, "refused") {
			t.Errorf("with the gate %s, a leak attempt got through: %s", gate, line)
		}
	}
	if len(lines) != leakAttempts {
		t.Errorf("with the gate %s, %d leak attempts ran, want %d:\n%s", gate, len(lines), leakAttempts, out)
	}
}

// expectLockedOut fails the test for everything a workstation gets
// through a gate that is locked.
func expectLockedOut(t *testing.T, sandboxID string) {
	t.Helper()
	// Loopback works whatever the gate says.
	out, code := inSandbox(sandboxID, `python3 -m http.server 8000 --bind 127.0.0.1 >/dev/null 2>&1 & sleep 1; curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8000/`)
	if code != 0 || trimmed(out) != "200" {
		t.Errorf("loopback request: %q (exit %d)", out, code)
	}
	// An external request fails, and at once rather than by timing out.
	began := time.Now()
	if out, code := inSandbox(sandboxID, fetch("https://1.1.1.1/")); code == 0 {
		t.Errorf("an external request succeeded through a locked gate: %q", out)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("the refusal took %v", took)
	}
	if got := attempt(t, sandboxID, "1.1.1.1", 80); got != refused {
		t.Errorf("a connection to a public address through a locked gate was %s, want %s", got, refused)
	}
	// No name is resolved: not by the sandbox's own resolver, not by
	// another.
	for _, server := range []string{testResolver.String(), "8.8.8.8"} {
		if out, _ := inSandbox(sandboxID, resolve+"example.com "+server); trimmed(out) != "no answer" {
			t.Errorf("resolving through %s with the gate locked answered %q", server, trimmed(out))
		}
	}
	expectNoLeak(t, sandboxID, "locked")
}

// download starts, in a sandbox, a download that takes minutes. It
// tells when the download ended, and the address it is fetched from.
func download(t *testing.T, sandboxID string) (<-chan time.Time, netip.Addr) {
	t.Helper()
	host := strings.TrimPrefix(publicDownloadURL, "https://")
	host = host[:strings.IndexByte(host, '/')]
	// A name query is one datagram, and one may be lost.
	var fields []string
	for try := 1; len(fields) == 0; try++ {
		out, _ := inSandbox(sandboxID, "getent ahostsv4 "+host+" | head -1")
		if fields = strings.Fields(out); len(fields) == 0 && try == 3 {
			status := updates.statusOf(sandboxID)
			t.Fatalf("the sandbox does not resolve %s: %q (it is %s %s)", host, out, status.State, status.LockReason)
		}
	}
	server, err := netip.ParseAddr(fields[0])
	if err != nil {
		t.Fatalf("the sandbox resolved %s to %q", host, fields[0])
	}
	// The download's server is asked again if it turns the download down
	// at once, as a busy one does.
	command := "curl -s --max-time 180 --resolve " + host + ":443:" + server.String() + " -o /dev/null " + publicDownloadURL
	for try := 1; ; try++ {
		done := make(chan time.Time, 1)
		go func() {
			inSandbox(sandboxID, command)
			done <- time.Now()
		}()
		select {
		case <-done:
			if try == 3 {
				status := updates.statusOf(sandboxID)
				t.Fatalf("the download did not run (the sandbox is %s %s)", status.State, status.LockReason)
			}
			// A gate that locked meanwhile ended it: the next is for
			// when the gate is open again.
			time.Sleep(time.Second)
			waitState(t, sandboxID, sandbox.StateGateOpen, 30*time.Second)
		case <-time.After(3 * time.Second):
			return done, server
		}
	}
}

// expectStopped fails the test if the sandbox still sends packets to
// the server of a download. A download whose packets no longer arrive is
// not reset: it sends nothing, so there is nothing to reset. That
// nothing more leaves for its server is what counts.
func expectStopped(t *testing.T, capture *tunCapture, server netip.Addr, why string) {
	t.Helper()
	settled := capture.sentTo(t, server)
	if settled == 0 {
		t.Errorf("the capture saw no packet of the download: it is not watching")
	}
	time.Sleep(3 * time.Second)
	if after := capture.sentTo(t, server); after != settled {
		t.Errorf("the sandbox sent %d packets of its download after %s", after-settled, why)
	}
}

// expectCut fails the test unless a download ended within a time of
// since.
func expectCut(t *testing.T, done <-chan time.Time, since time.Time, within time.Duration, why string) {
	t.Helper()
	select {
	case ended := <-done:
		if took := ended.Sub(since); took > within {
			t.Errorf("the download outlived %s by %v, want at most %v", why, took.Round(100*time.Millisecond), within)
		}
	case <-time.After(within + 20*time.Second):
		t.Fatalf("the download kept running after %s", why)
	}
}

// A gate nobody answers stays locked, and this needs no internet: the
// sandbox's only packets are its probe's.
func TestGateStaysLockedWhileItsProbeIsUnanswered(t *testing.T) {
	cfg := lockedConfig("gate-silent", "Silent")
	start(t, cfg, nil, nil)
	capture := captureTUN(t, cfg.ID)
	waitLockedFor(t, cfg.ID, sandbox.LockProbeUnanswered, 20*time.Second)

	expectLockedOut(t, cfg.ID)
	// A whole round of probes later it is as locked.
	time.Sleep(3 * time.Second)
	if status := updates.statusOf(cfg.ID); status.State != sandbox.StateGateLocked || status.LockReason != sandbox.LockProbeUnanswered {
		t.Errorf("status %+v, want locked for %q", status, sandbox.LockProbeUnanswered)
	}
	capture.expectOnly(t, silentProbe)
	expectTheDeviceIsTheGatekeepers(t, cfg.ID)
}

// expectTheDeviceIsTheGatekeepers fails the test unless the sandbox's
// namespace holds loopback and the TUN device and nothing else, and the
// workstation can neither make a device nor change the one there.
func expectTheDeviceIsTheGatekeepers(t *testing.T, sandboxID string) {
	t.Helper()
	if out, code := inSandbox(sandboxID, "ls /sys/class/net | sort | tr '\\n' ' '"); code != 0 || trimmed(out) != "lo "+sandbox.TUNDevice {
		t.Errorf("the sandbox's devices are %q, want loopback and %s", trimmed(out), sandbox.TUNDevice)
	}
	out, _ := inSandbox(sandboxID, `python3 -c '
import fcntl, os, socket, struct
def attempt(name, fn):
    try: print(name, fn())
    except OSError as e: print(name, "refused")
def open_the_device_node():
    os.open("/dev/net/tun", os.O_RDWR); return "OPENED"
def bring_the_device_down():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    fcntl.ioctl(s, 0x8914, struct.pack("16sH", b"tun0", 0)); return "DOWN"
def readdress_the_device():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    fcntl.ioctl(s, 0x8916, struct.pack("16sH2s4s8s", b"tun0", socket.AF_INET, b"", socket.inet_aton("10.9.9.9"), b"")); return "READDRESSED"
def change_its_mtu():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    fcntl.ioctl(s, 0x8922, struct.pack("16si", b"tun0", 9000)); return "CHANGED"
def add_a_route():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    fcntl.ioctl(s, 0x890B, bytes(120)); return "ADDED"
for n, f in list(globals().items()):
    if callable(f) and f.__module__ == "__main__" and n != "attempt": attempt(n, f)
'`)
	lines := strings.Split(trimmed(out), "\n")
	for _, line := range lines {
		if !strings.HasSuffix(line, "refused") {
			t.Errorf("the workstation could change the sandbox's way out: %s", line)
		}
	}
	if len(lines) != 5 {
		t.Errorf("%d attempts on the device ran, want 5:\n%s", len(lines), out)
	}
}

func TestGateLockedOnMismatch(t *testing.T) {
	exit := macExitAddress(t)
	cfg := openConfig(t, "gate-mismatch")
	cfg.AllowedInterfaces[0].ExpectedExitAddresses = []string{elsewhere}
	start(t, cfg, nil, nil)
	capture := captureTUN(t, cfg.ID)

	waitLockedFor(t, cfg.ID, sandbox.LockExitAddressMismatch, 30*time.Second)
	if status := updates.statusOf(cfg.ID); status.ExitAddress != exit {
		t.Errorf("the mismatch names %q, want the address this Mac's network leaves from", status.ExitAddress)
	}

	expectLockedOut(t, cfg.ID)
	// The probe's own port is as closed to the workstation: what leaves
	// for port 443 while the gate is locked is the probe's alone.
	other := netip.MustParseAddr("9.9.9.9")
	if got := attempt(t, cfg.ID, other.String(), 443); got != refused {
		t.Errorf("a connection to port 443 through a locked gate was %s, want %s", got, refused)
	}
	if n := capture.sentTo(t, other); n != 0 {
		t.Errorf("the sandbox sent %d packets to %s through a locked gate", n, other)
	}
	capture.expectProbesOnly(t)
}

func TestGateOpensOnTheExitAddress(t *testing.T) {
	exit := macExitAddress(t)
	met := make(chan bool, 1)
	holds := true
	conditionsMet := func() bool {
		select {
		case holds = <-met:
		default:
		}
		return holds
	}
	cfg := openConfig(t, "gate-open")
	cfg.HostConditions = []sandbox.HostCondition{{Kind: sandbox.WiFiSSID, Value: "Office"}}
	port, connections := macListener(t)
	start(t, cfg, nil, conditionsMet)
	capture := captureTUN(t, cfg.ID)

	status := waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	if status.ExitAddress != exit {
		t.Errorf("open on exit address %q, want %q", status.ExitAddress, exit)
	}
	// The workstation leaves from the address the probe saw, to servers
	// whose certificates it checks against the usual authorities.
	out, code := inSandbox(cfg.ID, fetch(publicProbeURL))
	if code != 0 || trimmed(out) != exit {
		t.Fatalf("the workstation's address as the internet sees it: %q (exit %d), want %s", trimmed(out), code, exit)
	}
	waitFor(t, 6*time.Second, "telemetry with packets passed and a probe latency", func() bool {
		tel := updates.gateOf(cfg.ID)
		return tel.PacketsPassed > 0 && tel.ProbeLatency > 0 && tel.ExitAddress == exit
	})
	// Ports other than the web's pass too.
	if out, _ := inSandbox(cfg.ID, banner(publicSSHHost)); !strings.Contains(out, "SSH-") {
		t.Errorf("%s on port 22 said %q, want an SSH banner", publicSSHHost, trimmed(out))
	}

	// Names are resolved by the sandbox's resolver, and by no other.
	if out, _ := inSandbox(cfg.ID, resolve+"example.com "+testResolver.String()); !strings.HasPrefix(trimmed(out), "rcode=0 ") || strings.Contains(out, "no data") {
		t.Errorf("resolving through %s with the gate open answered %q", testResolver, trimmed(out))
	}
	if out, _ := inSandbox(cfg.ID, resolve+"example.com 8.8.8.8"); trimmed(out) != "no answer" {
		t.Errorf("a resolver that is not the sandbox's answered %q", trimmed(out))
	}

	// What is not public stays out of reach while the gate is open:
	// nothing of the Mac, the VM or a private network answers, by address
	// or by name, and the Mac's own listener never sees a connection.
	// Where they are is read from the VM and from this Mac, not assumed.
	type target struct {
		what string
		host string
		port int
	}
	targets := []target{
		{"the Mac's listener through the host gateway", hostGateway(t).String(), port},
		{"the Mac's listener by Docker's name for it", "host.docker.internal", port},
		{"the Mac's listener by Lima's name for it", "host.lima.internal", port},
		{"the metadata address", "169.254.169.254", 80},
		{"a local name", "nas.local", 80},
		{"a private address", "10.0.0.1", 80},
		{"an address of the benchmarking range", "198.18.0.1", 80},
	}
	for _, addr := range vmAddresses(t) {
		targets = append(targets, target{"the VM at " + addr.String(), addr.String(), 22})
	}
	for _, addr := range macAddresses(t) {
		targets = append(targets, target{"the Mac's listener at one of the Mac's own addresses", addr.String(), port})
	}
	for _, tg := range targets {
		if got := attempt(t, cfg.ID, tg.host, tg.port); got == answered {
			t.Errorf("%s answered a connection from the sandbox", tg.what)
		}
	}
	if n := connections(); n != 0 {
		t.Errorf("the Mac's listener saw %d connections from the sandbox", n)
	}
	expectTheDeviceIsTheGatekeepers(t, cfg.ID)
	// The same attempt, at a public destination, is answered: the
	// attempts above could have succeeded.
	if got := attempt(t, cfg.ID, "one.one.one.one", 80); got != answered {
		t.Errorf("an attempt at a public destination was %s, want %s", got, answered)
	}
	// And the listener counts what does reach it.
	if c, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", itoa(port))); err == nil {
		c.Close()
	}
	waitFor(t, 3*time.Second, "the Mac's listener to count a connection from the Mac", func() bool { return connections() == 1 })

	// Every route but TCP and the resolver is as closed with the gate open.
	expectNoLeak(t, cfg.ID, "open")

	// A Host Condition fails: the gate locks at once and the download in
	// progress is cut, by the lease alone.
	done, _ := download(t, cfg.ID)
	met <- false
	unmet := time.Now()
	expectCut(t, done, unmet, 6*time.Second, "the Host Condition")
	if status := waitState(t, cfg.ID, sandbox.StateGateLocked, 5*time.Second); status.LockReason != sandbox.LockHostCondition {
		t.Errorf("locked for %q, want %q", status.LockReason, sandbox.LockHostCondition)
	}
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code == 0 {
		t.Errorf("a request succeeded while a Host Condition was unmet: %q", out)
	}

	// And it opens again.
	met <- true
	waitState(t, cfg.ID, sandbox.StateGateOpen, 15*time.Second)
	if out, code := inSandbox(cfg.ID, fetch(publicProbeURL)); code != 0 || trimmed(out) != exit {
		t.Errorf("after the gate opened again: %q (exit %d)", trimmed(out), code)
	}

	capture.expectGated(t)
}

// The timezone of the exit address is read before the workstation
// starts, which takes it as its own.
func TestTimezoneFollowsTheExitAddress(t *testing.T) {
	cfg := openConfig(t, "gate-timezone")
	cfg.Clock.Timezone = sandbox.TimezoneAuto
	cfg.Clock.TimezoneProbeURL = publicTimezoneURL
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)

	tz, _ := inSandbox(cfg.ID, "echo $TZ")
	if _, err := time.LoadLocation(trimmed(tz)); err != nil || trimmed(tz) == "" || trimmed(tz) == sandbox.TimezoneAuto {
		t.Errorf("the workstation's timezone is %q, want the IANA timezone of the exit address", trimmed(tz))
	}
}

// The gatekeeper is killed with a download in flight. The sandbox's
// device goes with it at once, tun having been one of its processes;
// its namespace outlives it for as long as the workstation does, with
// the ruleset still in it and nothing left to renew the lease. The
// sandbox's packets stop, and the sandbox ends.
func TestGatekeeperKilledMidDownload(t *testing.T) {
	cfg := openConfig(t, "gate-killed")
	start(t, cfg, nil, nil)
	capture := captureTUN(t, cfg.ID)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)

	// A download, and beside it a loop that keeps trying every other
	// route for as long as the workstation lives.
	download(t, cfg.ID)
	go inSandbox(cfg.ID, "while true; do python3 -c "+shellQuote(leakScript)+" >/dev/null 2>&1; done")
	before := len(capture.packets(t))
	time.Sleep(time.Second)
	if flowing := len(capture.packets(t)); flowing <= before {
		t.Fatalf("the capture saw %d packets, then %d: the download is not crossing the device", before, flowing)
	}

	if out, code := docker("kill", orchestrator.GatekeeperContainer(cfg.ID)); code != 0 {
		t.Fatal(out)
	}
	// What was already in flight may still cross for a moment; within
	// the lease's duration nothing does, and nothing ever again.
	time.Sleep(sandbox.LeaseDuration + time.Second)
	settled := len(capture.packets(t))
	time.Sleep(4 * time.Second)
	if after := len(capture.packets(t)); after != settled {
		t.Errorf("the sandbox sent %d packets after its gatekeeper was killed and its lease ran out", after-settled)
	}
	status := waitState(t, cfg.ID, sandbox.StateError, 15*time.Second)
	if !strings.Contains(status.Error, "gatekeeper") {
		t.Errorf("error %q", status.Error)
	}
	if _, code := docker("inspect", orchestrator.WorkstationContainer(cfg.ID)); code == 0 {
		t.Error("the workstation outlived its gatekeeper")
	}
	capture.expectGated(t)
}

// The gatekeeper's own process is stopped with a download in flight:
// nothing renews the lease and nothing ends it. Only the kernel is
// left, which lets the lease run out and resets the download at its
// next packet. tun runs on, so that it is the lease that stops the
// packets, and not the lack of anyone to move them.
func TestLeaseExpiresWithTheGatekeeperFrozen(t *testing.T) {
	cfg := openConfig(t, "gate-frozen")
	start(t, cfg, nil, nil)
	capture := captureTUN(t, cfg.ID)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)

	done, _ := download(t, cfg.ID)
	if out, code := docker("kill", "--signal", "STOP", orchestrator.GatekeeperContainer(cfg.ID)); code != 0 {
		t.Fatal(out)
	}
	defer docker("kill", "--signal", "CONT", orchestrator.GatekeeperContainer(cfg.ID))
	frozen := time.Now()
	if out, _ := docker("top", orchestrator.GatekeeperContainer(cfg.ID), "-o", "pid,stat,args"); !regexp.MustCompile(`(?m)^\s*\d+\s+[SR]\S*\s+.*/tun\s*$`).MatchString(out) || !regexp.MustCompile(`(?m)^\s*\d+\s+T\S*\s+.*gatekeeper run`).MatchString(out) {
		t.Fatalf("the gatekeeper's process is not stopped beside a tun that runs:\n%s", out)
	}
	expectCut(t, done, frozen, sandbox.LeaseDuration+2*time.Second, "its gatekeeper's freezing")

	// And nothing leaves while it stays frozen: not the workstation's
	// packets, and no probe.
	settled := len(capture.packets(t))
	if got := attempt(t, cfg.ID, "1.1.1.1", 80); got != refused {
		t.Errorf("with the gatekeeper frozen, a connection to a public address was %s, want %s", got, refused)
	}
	time.Sleep(3 * time.Second)
	if after := len(capture.packets(t)); after != settled {
		t.Errorf("the sandbox sent %d packets after its lease ran out with the gatekeeper frozen", after-settled)
	}
	capture.expectGated(t)
}

// carrierConnections are the TCP connections the Carrier of a sandbox
// holds on this Mac, as lsof lists them. The tests run one sandbox with
// a network at a time but where they say otherwise, so the Carrier is
// the newest of the test process's.
func carrierConnections(t *testing.T) string {
	t.Helper()
	pid := trimmed(commandOutput("pgrep", "-n", "-P", itoa(os.Getpid()), "-f", "bin/carrier"))
	if pid == "" {
		t.Fatal("the test process has no Carrier")
	}
	return commandOutput("lsof", "-nP", "-a", "-p", pid, "-iTCP")
}

// The gatekeeper's container is paused with a download in flight, and
// tun with it: nobody moves the sandbox's packets. The download gets no
// further at once, and the Carrier, told nothing of the gate for as long
// as a lease lasts, lets go of what it held for the sandbox.
func TestNothingLeavesWithTheGatekeepersContainerPaused(t *testing.T) {
	cfg := openConfig(t, "gate-paused")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)

	_, server := download(t, cfg.ID)
	if held := carrierConnections(t); !strings.Contains(held, "->"+server.String()+":443") {
		t.Fatalf("the Carrier holds no connection to the download's server:\n%s", held)
	}
	if out, code := docker("pause", orchestrator.GatekeeperContainer(cfg.ID)); code != 0 {
		t.Fatal(out)
	}
	defer docker("unpause", orchestrator.GatekeeperContainer(cfg.ID))
	waitFor(t, sandbox.LeaseDuration+5*time.Second, "the Carrier to let go of the download's connection", func() bool {
		return !strings.Contains(carrierConnections(t), "->"+server.String()+":443")
	})
	if got := attempt(t, cfg.ID, "1.1.1.1", 80); got == answered {
		t.Error("with the gatekeeper's container paused, a public address answered")
	}
}

// After a lock the Carrier holds nothing for the sandbox: the bytes of a
// download it had in hand do not go on leaving.
func TestTheCarrierHoldsNothingAfterALock(t *testing.T) {
	met := make(chan bool, 1)
	holds := true
	cfg := openConfig(t, "gate-let-go")
	cfg.HostConditions = []sandbox.HostCondition{{Kind: sandbox.WiFiSSID, Value: "Office"}}
	start(t, cfg, nil, func() bool {
		select {
		case holds = <-met:
		default:
		}
		return holds
	})
	waitState(t, cfg.ID, sandbox.StateGateOpen, 30*time.Second)
	_, server := download(t, cfg.ID)
	held := "->" + server.String() + ":443"
	if now := carrierConnections(t); !strings.Contains(now, held) {
		t.Fatalf("the Carrier holds no connection to the download's server:\n%s", now)
	}
	met <- false
	waitLockedFor(t, cfg.ID, sandbox.LockHostCondition, 5*time.Second)
	// The gatekeeper's word reaches the Carrier with its report: within
	// a moment of the lock the connection is gone, reset and not drained.
	waitFor(t, 2*time.Second, "the Carrier to hold no connection to the download's server after the lock", func() bool {
		return !strings.Contains(carrierConnections(t), held)
	})
}

// A sandbox whose Allowed Interfaces are none of this Mac's has no
// Interface in use: its gate is locked for it, and nothing at all
// reaches its device, not even a probe.
func TestNoInterfaceLocksTheGate(t *testing.T) {
	cfg := newConfig("gate-no-interface", "No Interface")
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: "nosuch0", AnyExitAddress: true}, {Name: "nosuch1", ExpectedExitAddresses: []string{elsewhere}}}
	start(t, cfg, nil, nil)
	capture := captureTUN(t, cfg.ID)
	waitLockedFor(t, cfg.ID, sandbox.LockNoInterface, 20*time.Second)
	if tel := updates.gateOf(cfg.ID); tel.Interface != "" || tel.Open {
		t.Errorf("with no Interface in use the gate says %+v", tel)
	}
	expectLockedOut(t, cfg.ID)
	expectTheDeviceIsTheGatekeepers(t, cfg.ID)
	// A whole round of probes later: still nothing.
	time.Sleep(3 * time.Second)
	if status := updates.statusOf(cfg.ID); status.State != sandbox.StateGateLocked || status.LockReason != sandbox.LockNoInterface {
		t.Errorf("status %+v, want locked for %q", status, sandbox.LockNoInterface)
	}
	capture.expectNothing(t)
	if held := carrierConnections(t); strings.Contains(held, "TCP") {
		t.Errorf("with no Interface in use the Carrier holds connections:\n%s", held)
	}
}

// The end of a sandbox's tun, or of its Carrier, is the end of that
// sandbox, and of no other.
func TestTheEndOfTunOrTheCarrierEndsThatSandboxAlone(t *testing.T) {
	a, b, c := lockedConfig("carried-a", "Carried A"), lockedConfig("carried-b", "Carried B"), lockedConfig("carried-c", "Carried C")
	for _, cfg := range []sandbox.Config{a, b, c} {
		start(t, cfg, nil, nil)
		waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)
	}
	// tun is killed in the first: its device goes, and the sandbox ends.
	if out, code := docker("exec", orchestrator.GatekeeperContainer(a.ID), "sh", "-c", "kill -9 $(pidof tun)"); code != 0 {
		t.Fatalf("killing tun: %s", out)
	}
	status := waitState(t, a.ID, sandbox.StateError, 15*time.Second)
	if !strings.Contains(status.Error, "Carrier stopped") {
		t.Errorf("after its tun was killed the sandbox says %q", status.Error)
	}
	if _, code := docker("inspect", orchestrator.WorkstationContainer(a.ID)); code == 0 {
		t.Error("a workstation outlived its sandbox's device")
	}
	// The Carrier of the third, the newest, is killed: that sandbox ends.
	pid := trimmed(commandOutput("pgrep", "-n", "-P", itoa(os.Getpid()), "-f", "bin/carrier"))
	if pid == "" {
		t.Fatal("the test process has no Carrier")
	}
	commandOutput("kill", "-9", pid)
	status = waitState(t, c.ID, sandbox.StateError, 15*time.Second)
	if !strings.Contains(status.Error, "Carrier stopped") {
		t.Errorf("after its Carrier was killed the sandbox says %q", status.Error)
	}
	if _, code := docker("inspect", orchestrator.WorkstationContainer(c.ID)); code == 0 {
		t.Error("a workstation outlived its sandbox's Carrier")
	}
	// The second saw none of it.
	time.Sleep(2 * time.Second)
	if status := updates.statusOf(b.ID); status.State != sandbox.StateGateLocked {
		t.Errorf("a sandbox whose tun and Carrier ran on is %+v", status)
	}
	expectTheDeviceIsTheGatekeepers(t, b.ID)
}

// fromPortScript connects to a public address from a source port it
// names, or listens on that port on loopback and connects to itself,
// and says how that ended.
const fromPortScript = `
import socket, sys
port, where = int(sys.argv[1]), sys.argv[2]
try:
    if where == "loopback":
        l = socket.socket(); l.bind(("127.0.0.1", port)); l.listen(1)
        socket.create_connection(("127.0.0.1", port), 4)
        print("answered")
    else:
        s = socket.socket(); s.settimeout(4); s.bind(("", port)); s.connect((where, 80))
        s.sendall(b"GET / HTTP/1.0\r\n\r\n")
        print("answered" if s.recv(64) else "closed")
except (ConnectionRefusedError, ConnectionResetError, BrokenPipeError):
    print("refused")
except socket.timeout:
    print("timed out")
except OSError as e:
    print("refused" if e.errno in (101, 113) else "error: %s" % e)
`

// The Probe Source Ports are the probe's alone: under an open gate,
// what the sandbox sends from one of them is refused, a connection from
// the port beside them is answered, and a server in the sandbox still
// listens on one, on loopback.
func TestTheProbeSourcePortsAreTheProbes(t *testing.T) {
	const id = "probe-ports"
	start(t, openConfig(t, id), nil, nil)
	waitState(t, id, sandbox.StateGateOpen, 30*time.Second)
	from := func(port int, where string) string {
		out, _ := inSandbox(id, "python3 -c "+shellQuote(fromPortScript)+" "+itoa(port)+" "+where)
		return trimmed(out)
	}
	for _, port := range []int{sandbox.ProbeSourcePortFirst, sandbox.ProbeSourcePortLast} {
		if got := from(port, "1.1.1.1"); got != refused {
			t.Errorf("a connection of the sandbox's from port %d was %s, want %s", port, got, refused)
		}
	}
	for _, port := range []int{sandbox.ProbeSourcePortFirst - 1, sandbox.ProbeSourcePortLast + 1} {
		if got := from(port, "1.1.1.1"); got != answered {
			t.Errorf("a connection of the sandbox's from port %d was %s, want %s", port, got, answered)
		}
	}
	if got := from(sandbox.ProbeSourcePortFirst, "loopback"); got != answered {
		t.Errorf("a server of the sandbox's on loopback, on port %d, was %s, want %s", sandbox.ProbeSourcePortFirst, got, answered)
	}
	if out, _ := inSandbox(id, "cat /proc/sys/net/ipv4/ip_local_port_range"); len(strings.Fields(out)) != 2 {
		t.Errorf("the sandbox's namespace hands out ports %q", trimmed(out))
	}
}
