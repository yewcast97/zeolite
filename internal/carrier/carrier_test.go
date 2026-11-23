package carrier

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// The test's own program stands in for a Carrier under the Carrier Wall:
// started with wallProbe set, it tries what the wall is there to refuse,
// says what came of each, and exits, before any test runs.
const wallProbe = "ZEOLITE_WALL_PROBE"

func TestMain(m *testing.M) {
	if port := os.Getenv(wallProbe); port != "" {
		probe(port, os.Getenv(wallProbe+"_FILE"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// probe tries, as a Carrier that had been taken over might, to do what
// is not a Carrier's to do.
func probe(port, file string) {
	say := func(what string, err error) {
		outcome := "ALLOWED"
		if err != nil {
			outcome = "refused"
		}
		fmt.Printf("probe: %s: %s\n", what, outcome)
	}
	_, err := os.ReadFile(file)
	say("reading a file of the user's", err)
	_, err = os.ReadDir(filepath.Dir(file))
	say("listing a folder of the user's", err)
	say("writing a file", os.WriteFile(file+".written", []byte("x"), 0o600))
	say("deleting a file", os.Remove(file))
	say("running a program", exec.Command("/bin/ls", "/").Run())
	_, err = net.Listen("tcp4", "127.0.0.1:0")
	say("listening on loopback", err)
	_, err = net.Listen("tcp4", "0.0.0.0:0")
	say("listening on every address", err)
	_, err = net.ListenPacket("udp4", "0.0.0.0:0")
	say("taking UDP from anyone", err)
	_, err = net.DialTimeout("tcp4", "127.0.0.1:"+port, 2*time.Second)
	say("connecting to this Mac's loopback", err)
	// Every other address this Mac has.
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			_, err = net.DialTimeout("tcp4", net.JoinHostPort(ipn.IP.String(), port), 2*time.Second)
			say("connecting to one of this Mac's own addresses", err)
		}
	}
	_, err = net.LookupHost("localhost.example.invalid")
	say("resolving a name", err)
	// What it was started with is its to use: its streams, and the two
	// descriptors the app hands it.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Printf("probe: its standard streams carried: %s", line)
	control, err2 := bufio.NewReader(os.NewFile(3, "control")).ReadString('\n')
	if err == nil && err2 == nil {
		fmt.Fprintf(os.NewFile(4, "reports"), "heard on 3: %s", control)
	}
}

// The Carrier Wall refuses what a Carrier has no business doing: files,
// programs, listening, and every address of this Mac itself.
func TestTheCarrierWallRefusesEverythingButConnectionsOut(t *testing.T) {
	wall, err := os.ReadFile("../../cmd/carrier/carrier.sb")
	if err != nil {
		t.Fatal(err)
	}
	self, err := filepath.EvalSymlinks(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	// A file of the user's, and a listener of this Mac's, for the probe
	// to try.
	file := filepath.Join(t.TempDir(), "precious.txt")
	if err := os.WriteFile(file, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var reached sync.WaitGroup
	connections := 0
	reached.Add(1)
	go func() {
		defer reached.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections++
			conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	controlRead, controlWrite, _ := os.Pipe()
	reportsRead, reportsWrite, _ := os.Pipe()
	cmd := exec.Command(sandboxExec, "-D", "CARRIER="+self, "-p", string(wall), self)
	cmd.Env = []string{wallProbe + "=" + port, wallProbe + "_FILE=" + file}
	cmd.Stdin = strings.NewReader("through the streams\n")
	cmd.ExtraFiles = []*os.File{controlRead, reportsWrite}
	go func() {
		controlWrite.WriteString("a judgement\n")
		controlWrite.Close()
	}()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the probe under the wall: %v\n%s", err, out)
	}
	reportsWrite.Close()
	reported, _ := io.ReadAll(reportsRead)

	tried := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		switch {
		case strings.HasSuffix(line, ": refused"):
			tried++
		case strings.HasSuffix(line, ": ALLOWED"):
			t.Errorf("under the Carrier Wall, %s", strings.TrimPrefix(line, "probe: "))
		}
	}
	if tried < 10 {
		t.Errorf("the probe tried %d things: the test would prove little\n%s", tried, out)
	}
	if !strings.Contains(string(out), "probe: its standard streams carried: through the streams\n") {
		t.Errorf("the probe's own streams did not carry:\n%s", out)
	}
	if string(reported) != "heard on 3: a judgement\n" {
		t.Errorf("the descriptors the app hands a Carrier carried %q", reported)
	}
	listener.Close()
	reached.Wait()
	if connections != 0 {
		t.Errorf("this Mac's listener got %d connections from under the wall", connections)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "mine" {
		t.Errorf("the user's file is %q (%v)", data, err)
	}
	if _, err := os.Lstat(file + ".written"); err == nil {
		t.Error("a file was written from under the wall")
	}
	// The control: without the wall, the same probe does those things.
	plain := exec.Command(self)
	plain.Env = cmd.Env
	plain.Stdin = strings.NewReader("\n")
	out, _ = plain.CombinedOutput()
	if n := strings.Count(string(out), ": ALLOWED"); n < 6 {
		t.Errorf("without the wall the probe was allowed %d things: it tries too little\n%s", n, out)
	}
}

func TestInstallPutsTheCarrierOnTheDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Application Support", "zeolite")
	program, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := embedded.ReadFile("bin/carrier-" + runtime.GOARCH)
	got, err := os.ReadFile(filepath.Join(dir, "bin", "carrier"))
	if err != nil || len(got) < 1<<20 || !bytes.Equal(got, want) {
		t.Fatalf("the Carrier on the disk is %d bytes (%v), want this Mac's, of %d", len(got), err, len(want))
	}
	// The wall the app starts it under is the one cmd/carrier pins.
	pinned, err := os.ReadFile("../../cmd/carrier/carrier.sb")
	if err != nil || program.wall != string(pinned) || !strings.Contains(program.wall, "(deny default)") {
		t.Errorf("the wall built into the app is not cmd/carrier's: run `go generate ./internal/carrier` (%v)", err)
	}
	before, _ := os.Lstat(program.path)
	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Lstat(program.path); !os.SameFile(before, after) {
		t.Error("a Carrier that was right was written again")
	}
	os.WriteFile(program.path, []byte("not a Carrier"), 0o600)
	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(program.path); !bytes.Equal(got, want) {
		t.Error("a Carrier that was wrong was left")
	}
	if info, _ := os.Lstat(program.path); info.Mode().Perm() != 0o755 {
		t.Errorf("the Carrier's mode is %v", info.Mode())
	}
}

// stream is a Packet stream whose sandbox end the test holds.
type stream struct {
	fromSandbox *io.PipeReader
	toSandbox   *io.PipeWriter
	// The test's end.
	write *io.PipeWriter
	read  *io.PipeReader
}

func newStream() *stream {
	s := &stream{}
	s.fromSandbox, s.write = io.Pipe()
	s.read, s.toSandbox = io.Pipe()
	return s
}

func (s *stream) Read(p []byte) (int, error)  { return s.fromSandbox.Read(p) }
func (s *stream) Write(p []byte) (int, error) { return s.toSandbox.Write(p) }
func (s *stream) Close() error {
	s.fromSandbox.Close()
	return s.toSandbox.Close()
}

// started is a real Carrier, under the wall, and what it told the app.
type started struct {
	c       *Carrier
	stream  *stream
	reports chan sandbox.CarrierReport
	ended   chan error
	mu      sync.Mutex
	said    []string
}

func start(t *testing.T, cfg sandbox.CarrierConfig) *started {
	t.Helper()
	program, err := Install(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &started{stream: newStream(), reports: make(chan sandbox.CarrierReport, 16), ended: make(chan error, 1)}
	s.c, err = program.Start(cfg, s.stream, Events{
		Report: func(r sandbox.CarrierReport) { s.reports <- r },
		Said: func(line string) {
			s.mu.Lock()
			s.said = append(s.said, line)
			s.mu.Unlock()
		},
		Ended: func(err error) { s.ended <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.c.Stop)
	return s
}

func (s *started) saidAll() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.said, "\n")
}

// A real Carrier, started as the app starts one: under the wall, with a
// Packet stream. Allowed loopback alone, it has an Interface in use that
// leads nowhere, so that nothing leaves this Mac.
func TestACarrierRunsUnderTheWall(t *testing.T) {
	s := start(t, sandbox.CarrierConfig{AllowedInterfaces: []string{"nothere0", "lo0"}, Resolver: "1.1.1.1"})
	select {
	case report := <-s.reports:
		if report != (sandbox.CarrierReport{Interface: "lo0", InterfaceChange: 1}) {
			t.Fatalf("the Carrier reports %+v, want loopback as the Interface in use", report)
		}
	case err := <-s.ended:
		t.Fatalf("the Carrier ended: %v\n%s", err, s.saidAll())
	case <-time.After(10 * time.Second):
		t.Fatalf("the Carrier reported no Interface in use\n%s", s.saidAll())
	}
	s.c.Judge(sandbox.CarrierJudgement{Open: true, Interface: "lo0", InterfaceChange: 1})

	// The sandbox tries a public address: bound to loopback there is no
	// way to it, and the sandbox hears nothing.
	syn := []byte{
		0x45, 0, 0, 40, 0, 1, 0, 0, 64, 6, 0, 0, 192, 0, 0, 2, 198, 51, 100, 20,
		0x9c, 0x40, 0x01, 0xbb, 0, 0, 0, 1, 0, 0, 0, 0, 0x50, 0x02, 0xff, 0xff, 0, 0, 0, 0,
	}
	frame, _ := sandbox.AppendPacket(nil, syn)
	if _, err := s.stream.write.Write(frame); err != nil {
		t.Fatal(err)
	}
	answered := make(chan int, 1)
	go func() {
		n, _ := s.stream.read.Read(make([]byte, 2048))
		answered <- n
	}()
	select {
	case n := <-answered:
		if n > 0 {
			t.Errorf("the sandbox heard %d bytes back about a connection that had no way out", n)
		}
	case <-time.After(time.Second):
	}

	// Stop ends it in order: it says what it carried, and the stream ends.
	s.c.Stop()
	select {
	case err := <-s.ended:
		if err != nil {
			t.Errorf("a stopped Carrier ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the Carrier did not end")
	}
	if said := s.saidAll(); !strings.Contains(said, "carrier: ended: carried 0 flows") {
		t.Errorf("the Carrier said:\n%s", said)
	}
	if _, err := s.stream.write.Write(frame); err == nil {
		t.Error("the Packet stream outlived its Carrier")
	}
	s.c.Judge(sandbox.CarrierJudgement{Open: true}) // to a Carrier that is gone: nothing
	s.c.Stop()
}

// A Carrier that ends by itself takes the sandbox's Packet stream with
// it: the sandbox's device goes, and nothing of it can leave.
func TestACarrierThatDiesEndsItsPacketStream(t *testing.T) {
	s := start(t, sandbox.CarrierConfig{AllowedInterfaces: []string{"lo0"}, Resolver: "1.1.1.1"})
	<-s.reports
	if err := s.c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-s.ended:
		if err == nil {
			t.Error("a Carrier that was killed is said to have ended in order")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the app did not hear that the Carrier was gone")
	}
	if _, err := s.stream.write.Write([]byte{0, 0}); err == nil {
		t.Error("the Packet stream outlived its Carrier")
	}

	// One that cannot take its configuration ends at once, and says why.
	bad := start(t, sandbox.CarrierConfig{AllowedInterfaces: []string{"lo0"}, Resolver: "192.168.0.1"})
	select {
	case err := <-bad.ended:
		if err == nil || !strings.Contains(bad.saidAll(), "the resolver is not a public IPv4 address") {
			t.Errorf("a Carrier with a private resolver ended with %v, saying:\n%s", err, bad.saidAll())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a Carrier with a configuration it cannot take went on")
	}
}
