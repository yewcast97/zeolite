//go:build integration

// Package integration tests zeolite against a real Docker daemon: the
// Colima profile `zeolite`. Run it with
//
//	go test -tags integration ./integration/
//
// A gate that never opens is tested without the internet. The tests of an
// open gate ask a public service what address they are seen at, and skip
// where this Mac reaches none. The tests of virtualinterface_test.go need
// a fixture that is not part of the repository.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zeolite/internal/carrier"
	"zeolite/internal/orchestrator"
	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
	"zeolite/internal/sandboximage"
)

// Addresses reserved for documentation: no host has one, so a packet to
// one is answered by nobody.
const (
	// elsewhere is an expected exit address that is nobody's.
	elsewhere = "203.0.113.7"
	// silentProbeURL is a probe URL that nobody answers.
	silentProbeURL = "https://192.0.2.1/"
)

// Endpoints on the public internet that the tests reach.
const (
	// publicProbeURL answers with the caller's address as plain text.
	publicProbeURL = "https://api.ipify.org"
	// publicTimezoneURL answers with the IANA timezone of the caller's address.
	publicTimezoneURL = "https://ipinfo.io/timezone"
	// publicDownloadURL is a file that takes minutes to download.
	publicDownloadURL = "https://ash-speed.hetzner.com/10GB.bin"
	// publicSSHHost answers a TCP connection on port 22 with a banner.
	publicSSHHost = "github.com"
)

// testResolver is the resolver the tests' sandboxes use.
var testResolver = netip.MustParseAddr(sandbox.DefaultResolver)

var (
	orch    *orchestrator.Orchestrator
	updates = &recorder{status: map[string]sandbox.Status{}}
)

// recorder keeps the Updates the orchestrator sends.
type recorder struct {
	mu          sync.Mutex
	status      map[string]sandbox.Status
	gate        map[string]sandbox.GateTelemetry
	workstation map[string]sandbox.WorkstationStatus
	// locks are the times a sandbox's open gate locked, and why.
	locks map[string][]locking
	// said is every status a sandbox had, in order, with when.
	said map[string][]statusAt
	// mcpCalls are the messages that crossed a sandbox's MCP bridges.
	mcpCalls map[string][]sandbox.MCPCall
	// hands are the Terminal Sessions whose agents raised their hands,
	// by sandbox, in order.
	hands map[string][]string
	// gateTaps are told every word of a sandbox's gate, as it comes.
	gateTaps map[string]func(time.Time, sandbox.GateTelemetry)
}

// tapGate has every word of a sandbox's gate told to tap, from now on.
func (r *recorder) tapGate(id string, tap func(time.Time, sandbox.GateTelemetry)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gateTaps == nil {
		r.gateTaps = map[string]func(time.Time, sandbox.GateTelemetry){}
	}
	r.gateTaps[id] = tap
}

// statusAt is a status a sandbox had from one moment on.
type statusAt struct {
	at     time.Time
	status sandbox.Status
}

// locking is one time an open gate locked.
type locking struct {
	at     time.Time
	reason sandbox.LockReason
}

func (r *recorder) listen(u sandbox.Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case u.Status != nil:
		if r.status[u.SandboxID].State == sandbox.StateGateOpen && u.Status.State == sandbox.StateGateLocked {
			if r.locks == nil {
				r.locks = map[string][]locking{}
			}
			r.locks[u.SandboxID] = append(r.locks[u.SandboxID], locking{time.Now(), u.Status.LockReason})
		}
		r.status[u.SandboxID] = *u.Status
		if r.said == nil {
			r.said = map[string][]statusAt{}
		}
		r.said[u.SandboxID] = append(r.said[u.SandboxID], statusAt{time.Now(), *u.Status})
	case u.Gate != nil:
		if r.gate == nil {
			r.gate = map[string]sandbox.GateTelemetry{}
		}
		r.gate[u.SandboxID] = *u.Gate
		if tap := r.gateTaps[u.SandboxID]; tap != nil {
			tap(time.Now(), *u.Gate)
		}
	case u.Workstation != nil:
		if r.workstation == nil {
			r.workstation = map[string]sandbox.WorkstationStatus{}
		}
		r.workstation[u.SandboxID] = *u.Workstation
	case u.Hand != nil:
		if r.hands == nil {
			r.hands = map[string][]string{}
		}
		r.hands[u.SandboxID] = append(r.hands[u.SandboxID], u.Hand.Session)
	case u.MCPCall != nil:
		if r.mcpCalls == nil {
			r.mcpCalls = map[string][]sandbox.MCPCall{}
		}
		r.mcpCalls[u.SandboxID] = append(r.mcpCalls[u.SandboxID], *u.MCPCall)
	}
}

// mcpCallsOf are the messages that crossed a sandbox's MCP bridges, in
// the order they were reported.
func (r *recorder) mcpCallsOf(id string) []sandbox.MCPCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sandbox.MCPCall(nil), r.mcpCalls[id]...)
}

// handsOf are the sessions of a sandbox whose agents raised their
// hands, in order.
func (r *recorder) handsOf(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hands[id]...)
}

// since returns the statuses a sandbox has had from a moment on, in
// order: also one it had for less time than a test takes to look.
func (r *recorder) since(id string, from time.Time) []sandbox.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []sandbox.Status
	for _, s := range r.said[id] {
		if !s.at.Before(from) {
			out = append(out, s.status)
		}
	}
	return out
}

func (r *recorder) statusOf(id string) sandbox.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status[id]
}

// noteLocks logs, when the test ends, each time the sandbox's open gate
// locked from now on, and what its gatekeeper said meanwhile of probes
// nobody answered. A lock ends every connection of the sandbox's: a
// test that fails for one the test did not cause says so with this.
func noteLocks(t *testing.T, id string) {
	t.Helper()
	began := time.Now()
	t.Cleanup(func() {
		updates.mu.Lock()
		locks := append([]locking(nil), updates.locks[id]...)
		updates.mu.Unlock()
		for _, l := range locks {
			if l.at.After(began) {
				t.Logf("%s: its open gate locked %v into the test: %s", id, l.at.Sub(began).Round(100*time.Millisecond), l.reason)
			}
		}
		// The gatekeeper writes its reports on standard output and what
		// went wrong on standard error, each line with its time here.
		out, _ := docker("logs", "--timestamps", "--since", began.UTC().Format(time.RFC3339Nano), orchestrator.GatekeeperContainer(id))
		for _, line := range strings.Split(out, "\n") {
			stamp, said, ok := strings.Cut(line, " gatekeeper: ")
			if !ok {
				continue
			}
			when := stamp
			if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
				when = at.Sub(began).Round(100 * time.Millisecond).String()
			}
			t.Logf("%s: its gatekeeper said %s into the test: %s", id, when, said)
		}
	})
}

func (r *recorder) gateOf(id string) sandbox.GateTelemetry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gate[id]
}

func (r *recorder) workstationOf(id string) (sandbox.WorkstationStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workstation[id]
	return w, ok
}

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	orch, err = orchestrator.New(ctx, orchestrator.ColimaEndpoint(), updates.listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration tests need the Colima profile `zeolite`:", err)
		os.Exit(1)
	}
	if err := orch.RemoveOrphans(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "removing orphans:", err)
		os.Exit(1)
	}
	if err := orch.LoadBackstop(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "loading the backstop:", err)
		os.Exit(1)
	}
	code := m.Run()
	orch.Close(ctx)
	os.Exit(code)
}

// docker runs the docker command against the test daemon and returns
// what it printed, with its exit code.
func docker(args ...string) (string, int) { return dockerWith(nil, "", args...) }

// dockerWith is docker with variables added to the command's own
// environment, and text on its standard input: what must be on no
// command line, where every program of this Mac reads it, crosses so.
func dockerWith(env []string, stdin string, args ...string) (string, int) {
	cmd := exec.Command("docker", append([]string{"-H", orchestrator.ColimaEndpoint()}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return out.String(), code
}

// inSandbox runs a shell command in a sandbox's workstation as the agent.
func inSandbox(id, script string) (string, int) {
	return docker("exec", orchestrator.WorkstationContainer(id), "sh", "-c", script)
}

// inLoginShell runs a shell command in a sandbox's workstation as a
// Terminal Session's shell would: in a login shell, which finds what the
// workstation's user installed under the home.
func inLoginShell(id, script string) (string, int) {
	return docker("exec", "-w", "/workspace", orchestrator.WorkstationContainer(id), "bash", "--login", "-c", script)
}

// start starts a sandbox, and stops it when the test ends.
func start(t *testing.T, cfg sandbox.Config, packs []pack.Manifest, conditionsMet func() bool) {
	t.Helper()
	startWith(t, cfg, packs, orchestrator.StartOptions{ConditionsMet: conditionsMet})
}

// theCarrier is the Carrier's program, put on the disk once for the
// tests as the app puts it there at launch.
var theCarrier = sync.OnceValues(func() (*carrier.Program, error) {
	dir, err := os.MkdirTemp("", "zeolite-test-carrier-")
	if err != nil {
		return nil, err
	}
	return carrier.Install(dir)
})

// carry starts a sandbox's real Carrier, under the Carrier Wall, as the
// app does.
func carry(t *testing.T, sandboxID string) func(sandbox.CarrierConfig, io.ReadWriteCloser, func(sandbox.CarrierReport), func(error)) (orchestrator.Carrier, error) {
	return func(cfg sandbox.CarrierConfig, stream io.ReadWriteCloser, report func(sandbox.CarrierReport), ended func(error)) (orchestrator.Carrier, error) {
		program, err := theCarrier()
		if err != nil {
			return nil, err
		}
		c, err := program.Start(cfg, stream, carrier.Events{
			Report: report,
			Said:   func(line string) { t.Logf("%s: %s", sandboxID, line) },
			Ended:  ended,
		})
		return c, err
	}
}

// appSandboxID is the shape of the ID the app gives a sandbox. A test's
// sandbox has another: no test deletes the Home of a sandbox of the
// app's.
var appSandboxID = regexp.MustCompile(`^s[0-9a-f]{10}$`)

// freshHome deletes the Home of a test's sandbox, now and when the test
// ends, after its sandbox was stopped: a test starts on an empty Home,
// as a new sandbox does, and leaves none in the VM. A Home is deleted
// by the test's own ID alone: nothing here lists volumes.
func freshHome(t *testing.T, sandboxID string) {
	t.Helper()
	if appSandboxID.MatchString(sandboxID) {
		t.Fatalf("%q is shaped like the ID of a sandbox of the app's: no test deletes such a Home", sandboxID)
	}
	remove := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := orch.RemoveHome(ctx, sandboxID); err != nil {
			t.Errorf("deleting the Home of the test's sandbox %s: %v", sandboxID, err)
		}
	}
	remove()
	// Registered before the sandbox's stop, so that it runs after it.
	t.Cleanup(remove)
}

// startWith is start with everything a sandbox can be given, on an
// empty Home. A sandbox that is given no Carrier gets the real one.
func startWith(t *testing.T, cfg sandbox.Config, packs []pack.Manifest, opts orchestrator.StartOptions) {
	t.Helper()
	freshHome(t, cfg.ID)
	startAgain(t, cfg, packs, opts)
}

// startAgain starts a sandbox on the Home it has, as the app starts one
// that was stopped, and stops it when the test ends. The image of its
// workstation is built first where it is not there, by a sandbox whose
// gate opens: the test's own may be one whose gate never does.
func startAgain(t *testing.T, cfg sandbox.Config, packs []pack.Manifest, opts orchestrator.StartOptions) {
	t.Helper()
	built(t, packs)
	startUnbuilt(t, cfg, packs, opts)
}

// workstationImage is the tag of the image of a workstation with these
// packs, in the test daemon.
func workstationImage(t *testing.T, packs []pack.Manifest) string {
	t.Helper()
	arch, code := docker("version", "--format", "{{.Server.Arch}}")
	if code != 0 {
		t.Fatalf("asking the test daemon for its architecture: %s", arch)
	}
	image, err := sandboximage.Workstation(trimmed(arch), packs)
	if err != nil {
		t.Fatal(err)
	}
	return image.Tag
}

// built makes sure the image of a workstation with these packs is
// there. An image is built by way of its sandbox's gate, so a sandbox
// whose gate never opens builds none: where the image is not there, a
// sandbox whose gate opens is started for it, and stopped. A test of a
// gate that stays locked then needs the internet only when its image is
// new, and skips where there is none.
func built(t *testing.T, packs []pack.Manifest) {
	t.Helper()
	if _, code := docker("image", "inspect", workstationImage(t, packs)); code == 0 {
		return
	}
	cfg := openConfig(t, "builder")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	err := orch.Start(ctx, cfg, packs, orchestrator.StartOptions{Carry: carry(t, cfg.ID)})
	orch.Stop(context.Background(), cfg.ID)
	if removeErr := orch.RemoveHome(context.Background(), cfg.ID); removeErr != nil {
		t.Errorf("deleting the Home of the sandbox that built an image: %v", removeErr)
	}
	if err != nil {
		t.Fatalf("building the image of a workstation by way of an open gate: %v", err)
	}
}

// startUnbuilt is startAgain without the image built first: the
// sandbox builds its own, by way of its own gate, where it is not there.
func startUnbuilt(t *testing.T, cfg sandbox.Config, packs []pack.Manifest, opts orchestrator.StartOptions) {
	t.Helper()
	if opts.Carry == nil {
		opts.Carry = carry(t, cfg.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	t.Cleanup(func() { orch.Stop(context.Background(), cfg.ID) })
	if err := orch.Start(ctx, cfg, packs, opts); err != nil {
		t.Fatalf("starting %s: %v", cfg.Name, err)
	}
}

// newConfig is a new sandbox's config, but that its gate keeps no Hold:
// most tests are of a gate that cuts, and a test of a Hold says so.
func newConfig(id, name string) sandbox.Config {
	cfg := sandbox.NewConfig(id, name)
	cfg.Gate.Hold = false
	return cfg
}

// lockedConfig is a sandbox whose gate never opens:
// nobody answers its probe. Its one Allowed Interface is this Mac's
// loopback, which is up, has an address, and leads to no public address:
// nothing of the sandbox leaves this Mac. It is all a test of a pod's
// lifecycle needs, and it runs where there is no internet.
func lockedConfig(id, name string) sandbox.Config {
	cfg := newConfig(id, name)
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: "lo0", ExpectedExitAddresses: []string{elsewhere}}}
	cfg.Gate.ProbeURLs = []string{silentProbeURL}
	return cfg
}

// silentProbe is where the packets of lockedConfig's probe go.
var silentProbe = netip.MustParseAddrPort("192.0.2.1:443")

// seenAt asks the public probe URL, from this test process, what address
// it is called from: the exit address of this Mac's network, as it is.
// Its transport is a new one, which takes no setting from the environment:
// it uses this Mac's network as a sandbox's packets do.
var seenAt = sync.OnceValues(func() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := client.Get(publicProbeURL)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64))
	if err != nil {
		return "", err
	}
	addr, err := sandbox.ParsePublicIPv4(string(body))
	if err != nil {
		return "", fmt.Errorf("%s answered %q: %w", publicProbeURL, body, err)
	}
	return addr.String(), nil
})

// macExitAddress is the exit address of this Mac's network. A test that
// needs the internet skips where there is none.
func macExitAddress(t *testing.T) string {
	t.Helper()
	addr, err := seenAt()
	if err != nil {
		t.Skipf("this Mac does not reach %s: %v", publicProbeURL, err)
	}
	return addr
}

// physicalInterface is the interface of this Mac that a socket bound to none
// leaves by: the one the test process's own requests take.
var physicalInterface = sync.OnceValues(func() (string, error) {
	conn, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.UDPAddr).IP
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifi := range interfaces {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(local) {
				return ifi.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no interface of this Mac has %s", local)
})

// openConfig is a sandbox whose gate opens where this
// Mac's network is as the test process sees it: it may leave by the
// interface the test process's own requests take, and is expected at
// the address they are seen at.
func openConfig(t *testing.T, id string) sandbox.Config {
	t.Helper()
	exit := macExitAddress(t)
	name, err := physicalInterface()
	if err != nil {
		t.Skipf("this Mac's physical Interface could not be found: %v", err)
	}
	cfg := newConfig(id, "Open "+id)
	cfg.AllowedInterfaces = []sandbox.AllowedInterface{{Name: name, ExpectedExitAddresses: []string{exit}}}
	return cfg
}

// waitFor polls until ok, failing the test with what after timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitState waits for a sandbox's status to reach a state.
func waitState(t *testing.T, id string, state sandbox.State, timeout time.Duration) sandbox.Status {
	t.Helper()
	waitFor(t, timeout, fmt.Sprintf("sandbox %s to be %s (it is %+v)", id, state, updates.statusOf(id)), func() bool {
		return updates.statusOf(id).State == state
	})
	return updates.statusOf(id)
}

// How a connection attempt from a workstation ended.
const (
	// answered: the iface end sent bytes back. Something was reached.
	answered = "answered"
	// closed: the connection opened and ended without a byte.
	closed = "closed"
	// refused: the connection was reset before it opened.
	refused = "refused"
	// timedOut: nothing came back at all.
	timedOut = "timed out"
	// unresolved: the name has no address.
	unresolved = "unresolved"
)

// attemptScript connects to a host and port, sends an HTTP request line
// and waits for anything to come back.
const attemptScript = `
import socket, sys
host, port = sys.argv[1], int(sys.argv[2])
try:
    s = socket.create_connection((host, port), 4)
    s.settimeout(4)
    s.sendall(b"GET / HTTP/1.0\r\n\r\n")
    print("answered" if s.recv(64) else "closed")
except socket.gaierror:
    print("unresolved")
except (ConnectionRefusedError, ConnectionResetError, BrokenPipeError):
    print("refused")
except socket.timeout:
    print("timed out")
except OSError as e:
    print("refused" if e.errno in (101, 113) else "error: %s" % e)
`

// attempt tries a TCP connection from a sandbox's workstation and says
// how it ended. A test of what a sandbox cannot reach asks this, and not
// whether curl failed: curl fails just as well against a server that
// answered in a protocol it does not speak.
func attempt(t *testing.T, sandboxID, host string, port int) string {
	t.Helper()
	out, code := inSandbox(sandboxID, "python3 -c "+shellQuote(attemptScript)+" "+shellQuote(host)+" "+fmt.Sprint(port))
	result := trimmed(out)
	switch result {
	case answered, closed, refused, timedOut, unresolved:
		return result
	}
	t.Fatalf("a connection attempt to %s:%d did not run: %q (exit %d)", host, port, out, code)
	return ""
}

// banner is a command line that prints what a host says first to a TCP
// connection on its SSH port, or why it said nothing.
func banner(host string) string {
	return fmt.Sprintf(`python3 -c '
import socket
try:
    s = socket.create_connection(("%s", 22), 8); s.settimeout(8); print(s.recv(32))
except OSError as e:
    print("nothing:", e)
'`, host)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

// helper is the backstop's helper container, which sits on the VM's own
// network: what the VM is is read from it, not written down here.
const helper = "zeolite-backstop"

// hostGatewayOnce asks Docker for the address by which the VM reaches
// the Mac. Docker tells it to a container that asks; no container of the
// app's does.
var hostGatewayOnce = sync.OnceValues(func() (netip.Addr, string) {
	image, code := docker("inspect", "-f", "{{.Config.Image}}", helper)
	if code != 0 {
		return netip.Addr{}, "the helper has no image: " + image
	}
	out, code := docker("run", "--rm", "--network", "none", "--add-host", "host.docker.internal:host-gateway", "--entrypoint", "getent", trimmed(image), "hosts", "host.docker.internal")
	fields := strings.Fields(out)
	if code != 0 || len(fields) == 0 {
		return netip.Addr{}, "Docker does not say how containers reach the Mac: " + out
	}
	addr, err := netip.ParseAddr(fields[0])
	if err != nil {
		return netip.Addr{}, "the host gateway is " + fields[0]
	}
	return addr, ""
})

// hostGateway is the address by which the VM reaches the Mac.
func hostGateway(t *testing.T) netip.Addr {
	t.Helper()
	addr, problem := hostGatewayOnce()
	if problem != "" {
		t.Fatal(problem)
	}
	return addr
}

// vmAddresses are the VM's own addresses: the one on the network it
// shares with the Mac, and its side of every container bridge.
func vmAddresses(t *testing.T) []netip.Addr {
	t.Helper()
	out, code := docker("exec", helper, "hostname", "-I")
	var addrs []netip.Addr
	for _, f := range strings.Fields(out) {
		if addr, err := netip.ParseAddr(f); err == nil && addr.Is4() {
			addrs = append(addrs, addr)
		}
	}
	if code != 0 || len(addrs) == 0 {
		t.Fatalf("the helper does not know the VM's addresses: %s", out)
	}
	return addrs
}

// macListener serves HTTP on every address this Mac has, as a dev server
// that listens too widely would: something no sandbox may reach, by
// loopback or by any other address of this Mac. It counts every
// connection that arrives, whatever is said on it.
func macListener(t *testing.T) (port int, connections func() int) {
	t.Helper()
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "the Mac's private service")
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				n.Add(1)
			}
		},
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port, func() int { return int(n.Load()) }
}

func itoa(n int) string { return strconv.Itoa(n) }

// fetch is a curl command line for a URL, which fails where the URL's
// server did not answer with success.
func fetch(url string) string {
	return "curl -sS -f --max-time 8 " + url
}

func trimmed(s string) string { return strings.TrimSpace(s) }
