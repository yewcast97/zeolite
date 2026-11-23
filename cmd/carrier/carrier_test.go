//go:build darwin

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"zeolite/internal/sandbox"
)

// The tests need neither the internet nor root. The sandbox's side of
// the Packet stream is a second network stack, in the test, that has the
// sandbox's address; this Mac's interfaces, the bound dial and the time
// are the test's to say.

// Addresses of the tests: public ones that stand for the internet, and
// what this Mac's interfaces have.
var (
	farEnd   = netip.MustParseAddrPort("198.51.100.20:443")
	resolver = netip.MustParseAddr("1.1.1.1")
	exit     = netip.MustParseAddr("203.0.113.9")
)

// dialed is one socket the Carrier asked for.
type dialed struct {
	index   int
	network string
	address string
}

// rig is a Carrier with a sandbox on its Packet stream.
type rig struct {
	t *testing.T
	c *carrier
	// sandbox is the sandbox's network stack, and toCarrier the Packet
	// stream as the sandbox writes it.
	sandbox   *stack.Stack
	toCarrier io.Writer

	mu         sync.Mutex
	interfaces []seenInterface
	dials      []dialed
	// answer says how the far end takes a connection to an address: it
	// returns the connection, or why there is none.
	answer  func(ctx context.Context, d dialed) (net.Conn, error)
	reports []sandbox.CarrierReport
	now     time.Time
	// probes counts the connections made as a probe's.
	probes int
	// served are the far ends' sides of the connections serve answered:
	// a server keeps its side until the test is over, whether or not the
	// test looks at it.
	served []net.Conn
}

// tunnel and ethernet are two interfaces this Mac may have.
func tunnel(index int) seenInterface {
	return seenInterface{Name: "utun9", Index: index, Up: true, Addrs: []netip.Addr{netip.MustParseAddr("198.18.0.1")}}
}

func ethernet() seenInterface {
	return seenInterface{Name: "en0", Index: 5, Up: true, Addrs: []netip.Addr{netip.MustParseAddr("192.168.0.7"), netip.MustParseAddr("198.51.100.7")}}
}

// newRig starts a Carrier whose sandbox may leave by utun9, then en0.
func newRig(t *testing.T, interfaces ...seenInterface) *rig {
	t.Helper()
	return newRigOf(t, sandbox.CarrierConfig{
		AllowedInterfaces:     []string{"utun9", "en0"},
		Resolver:              resolver.String(),
		ExpectedExitAddresses: []string{exit.String()},
	}, interfaces...)
}

// newRigOf starts a Carrier with a config.
func newRigOf(t *testing.T, cfg sandbox.CarrierConfig, interfaces ...seenInterface) *rig {
	t.Helper()
	c, err := newCarrier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, c: c, interfaces: interfaces, now: time.Unix(1_000_000, 0)}
	c.look = func() []seenInterface {
		r.mu.Lock()
		defer r.mu.Unlock()
		return append([]seenInterface(nil), r.interfaces...)
	}
	c.dialBound = func(ctx context.Context, index int, network, address string) (net.Conn, error) {
		d := dialed{index, network, address}
		r.mu.Lock()
		r.dials = append(r.dials, d)
		answer := r.answer
		r.mu.Unlock()
		if answer == nil {
			return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		}
		return answer(ctx, d)
	}
	c.now = func() time.Time {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.now
	}
	c.report = func(report sandbox.CarrierReport) {
		r.mu.Lock()
		r.reports = append(r.reports, report)
		r.mu.Unlock()
	}

	// The sandbox: one address, and everything routed to its device.
	r.sandbox = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	device := channel.New(1024, sandbox.PacketMTU, "")
	if err := r.sandbox.CreateNIC(1, device); err != nil {
		t.Fatal(err)
	}
	address := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4(sandbox.SandboxAddress.As4()).WithPrefix()}
	if err := r.sandbox.AddProtocolAddress(1, address, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	r.sandbox.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	// A port the sandbox picks for itself is never one of the Probe Source
	// Ports: the workstation's ruleset rejects those for anything without
	// the probe's mark, so the Carrier sees one only from the probe.
	first, _ := r.sandbox.PortRange()
	if err := r.sandbox.SetPortRange(first, sandbox.ProbeSourcePortFirst-1); err != nil {
		t.Fatal(err)
	}

	fromSandbox, toCarrier := io.Pipe()
	fromCarrier, toSandbox := io.Pipe()
	r.toCarrier = toCarrier
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.carry(ctx, fromSandbox, toSandbox)
	}()
	// The sandbox's device to the Packet stream, and back.
	go func() {
		for {
			pkt := device.ReadContext(ctx)
			if pkt == nil {
				return
			}
			view := pkt.ToView()
			pkt.DecRef()
			frame, _ := sandbox.AppendPacket(nil, view.AsSlice())
			view.Release()
			if _, err := toCarrier.Write(frame); err != nil {
				return
			}
		}
	}()
	go func() {
		reader := bufio.NewReader(fromCarrier)
		packet := make([]byte, sandbox.MaxPacket)
		for {
			n, err := sandbox.ReadPacket(reader, packet)
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet[:n])})
			device.InjectInbound(header.IPv4ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		toCarrier.Close()
		fromCarrier.Close()
		<-done
		r.sandbox.Close()
	})
	return r
}

// look has the Carrier look at this Mac's interfaces, a moment later.
func (r *rig) look() {
	r.pass(lookEvery)
	r.c.lookNow(r.now)
}

// pass moves the Carrier's clock on.
func (r *rig) pass(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

// set changes what interfaces this Mac has, and has the Carrier look.
func (r *rig) set(interfaces ...seenInterface) {
	r.mu.Lock()
	r.interfaces = interfaces
	r.mu.Unlock()
	r.look()
}

// lastReport is what the Carrier last said of the Interface in use.
func (r *rig) lastReport() sandbox.CarrierReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reports) == 0 {
		return sandbox.CarrierReport{}
	}
	return r.reports[len(r.reports)-1]
}

// acknowledge judges the gate as the gatekeeper would once it has taken
// note of what the Carrier last reported: locked, or open by a lease
// that has its whole duration to run.
func (r *rig) acknowledge(open bool) {
	report := r.lastReport()
	j := sandbox.CarrierJudgement{Open: open, Interface: report.Interface, InterfaceChange: report.InterfaceChange}
	if open {
		j.LeaseFor = sandbox.LeaseDuration
	}
	r.c.judge(j, r.now)
}

// hold judges the gate locked with a Hold that lasts.
func (r *rig) hold() {
	report := r.lastReport()
	r.c.judge(sandbox.CarrierJudgement{Held: true, Interface: report.Interface, InterfaceChange: report.InterfaceChange}, r.now)
}

// dialsTo are the sockets the Carrier asked for, to one address.
func (r *rig) dialsTo(address string) []dialed {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []dialed
	for _, d := range r.dials {
		if d.address == address {
			out = append(out, d)
		}
	}
	return out
}

func (r *rig) dialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dials)
}

// serve has the far end answer every connection with a server of the
// test's, and returns that server's side of each.
func (r *rig) serve() <-chan net.Conn {
	accepted := make(chan net.Conn, 64)
	r.mu.Lock()
	r.answer = func(ctx context.Context, d dialed) (net.Conn, error) {
		near, far, err := socketPair()
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.served = append(r.served, far)
		r.mu.Unlock()
		accepted <- far
		return near, nil
	}
	r.mu.Unlock()
	r.t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, far := range r.served {
			far.Close()
		}
	})
	return accepted
}

// socketPair is a connection and its far end, as a socket of this Mac's
// is: one whose side can end what it sends and go on reading, which a
// net.Pipe cannot. The far end then reads that end, as a server reads a
// FIN.
func socketPair() (near, far net.Conn, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	ends := make([]net.Conn, 2)
	for i, fd := range fds {
		file := os.NewFile(uintptr(fd), "socketpair")
		ends[i], err = net.FileConn(file)
		file.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return ends[0], ends[1], nil
}

// connect makes a connection from the sandbox, waiting at most so long.
func (r *rig) connect(dst netip.AddrPort, wait time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return gonet.DialContextTCP(ctx, r.sandbox, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(dst.Addr().As4()), Port: dst.Port()}, ipv4.ProtocolNumber)
}

// probe makes a connection as the gatekeeper's probe does: from the
// sandbox's namespace, and from the next of the Probe Source Ports.
func (r *rig) probe(dst netip.AddrPort, wait time.Duration) (net.Conn, error) {
	r.mu.Lock()
	port := uint16(sandbox.ProbeSourcePortFirst + r.probes)
	r.probes++
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	from := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(sandbox.SandboxAddress.As4()), Port: port}
	return gonet.DialTCPWithBind(ctx, r.sandbox, from, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(dst.Addr().As4()), Port: dst.Port()}, ipv4.ProtocolNumber)
}

// silence is how long the tests wait to say that the sandbox heard
// nothing.
const silence = 400 * time.Millisecond

// heardNothing reports whether an attempt to connect neither succeeded
// nor was refused: the sandbox's packets went unanswered.
func heardNothing(err error) bool {
	return err != nil && !errors.Is(err, syscall.ECONNREFUSED) && !strings.Contains(err.Error(), "refused")
}

// ready is a rig whose Interface in use is utun9, acknowledged, with
// the gate open.
func ready(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, tunnel(20), ethernet())
	r.look()
	if got := r.lastReport(); got != (sandbox.CarrierReport{Interface: "utun9", InterfaceChange: 1}) {
		t.Fatalf("the Carrier first reports %+v", got)
	}
	r.acknowledge(true)
	return r
}

// packet builds an IPv4 packet with a transport header's first bytes.
func packet(protocol byte, src, dst netip.Addr, dstPort uint16, change func([]byte)) []byte {
	p := make([]byte, 28)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	p[8], p[9] = 64, protocol
	copy(p[12:16], src.AsSlice())
	copy(p[16:20], dst.AsSlice())
	binary.BigEndian.PutUint16(p[20:22], 40000)
	binary.BigEndian.PutUint16(p[22:24], dstPort)
	if change != nil {
		change(p)
	}
	return p
}

// Only IPv4 TCP, and name queries to the sandbox's resolver, from the
// sandbox's own address, are given to the Carrier's stack to read.
func TestOnlyWhatTheCarrierReadsReachesItsStack(t *testing.T) {
	own := sandbox.SandboxAddress
	web := farEnd.Addr()
	for what, c := range map[string]struct {
		packet []byte
		want   bool
	}{
		"TCP from the sandbox":                   {packet(6, own, web, 443, nil), true},
		"TCP to a private address":               {packet(6, own, netip.MustParseAddr("10.0.0.1"), 22, nil), true}, // read, and then not carried
		"a name query to the resolver":           {packet(17, own, resolver, 53, nil), true},
		"UDP to the resolver on another port":    {packet(17, own, resolver, 443, nil), false},
		"a name query to another resolver":       {packet(17, own, netip.MustParseAddr("8.8.8.8"), 53, nil), false},
		"QUIC":                                   {packet(17, own, web, 443, nil), false},
		"a ping":                                 {packet(1, own, web, 0, nil), false},
		"another protocol":                       {packet(47, own, web, 0, nil), false},
		"TCP that claims another source":         {packet(6, netip.MustParseAddr("192.0.0.3"), web, 443, nil), false},
		"TCP that claims this Mac's address":     {packet(6, netip.MustParseAddr("192.168.0.7"), web, 443, nil), false},
		"a fragment with more to come":           {packet(6, own, web, 443, func(p []byte) { p[6] = 0x20 }), false},
		"a fragment that is not the first":       {packet(6, own, web, 443, func(p []byte) { p[7] = 0x08 }), false},
		"IPv6":                                   {packet(6, own, web, 443, func(p []byte) { p[0] = 0x60 }), false},
		"a header shorter than a header":         {packet(6, own, web, 443, func(p []byte) { p[0] = 0x44 }), false},
		"a header longer than the packet":        {packet(6, own, web, 443, func(p []byte) { p[0] = 0x4f }), false},
		"a length longer than the packet":        {packet(6, own, web, 443, func(p []byte) { binary.BigEndian.PutUint16(p[2:4], 1500) }), false},
		"a length shorter than the header":       {packet(6, own, web, 443, func(p []byte) { binary.BigEndian.PutUint16(p[2:4], 12) }), false},
		"a name query cut before its ports":      {packet(17, own, resolver, 53, nil)[:22], false},
		"a name query whose length hides a port": {packet(17, own, resolver, 53, func(p []byte) { binary.BigEndian.PutUint16(p[2:4], 22) }), false},
		"nothing":                                {nil, false},
		"a few bytes":                            {[]byte{0x45, 0, 0}, false},
	} {
		if got := carried(c.packet, resolver); got != c.want {
			t.Errorf("%s: read %v, want %v", what, got, c.want)
		}
	}

	// And what is not read never becomes a socket, whatever is sent.
	r := ready(t)
	r.serve()
	before := r.c.droppedPackets.Load()
	for _, p := range [][]byte{
		packet(1, own, web, 0, nil),
		packet(17, own, web, 443, nil),
		packet(17, own, netip.MustParseAddr("8.8.8.8"), 53, nil),
		packet(6, netip.MustParseAddr("192.168.0.7"), web, 443, nil),
	} {
		frame, _ := sandbox.AppendPacket(nil, p)
		r.toCarrier.Write(frame)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.c.droppedPackets.Load() < before+4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.c.droppedPackets.Load() - before; got != 4 {
		t.Errorf("of four packets that are not the Carrier's to read, %d were dropped unread", got)
	}
	if n := r.dialCount(); n != 0 {
		t.Errorf("packets that are not the Carrier's to read made %d sockets", n)
	}
}

func TestAConnectionIsCarriedOnTheInterfaceInUse(t *testing.T) {
	r := newRig(t, tunnel(20), ethernet())
	accepted := r.serve()
	r.look()
	if got := r.lastReport(); got != (sandbox.CarrierReport{Interface: "utun9", InterfaceChange: 1}) {
		t.Fatalf("the Carrier first reports %+v, want the most preferred Interface that is there", got)
	}
	// Until the gatekeeper has taken note of the Interface, nothing is
	// carried, and the sandbox hears nothing.
	if _, err := r.connect(farEnd, silence); !heardNothing(err) || r.dialCount() != 0 {
		t.Fatalf("before the gatekeeper acknowledged the Interface, a connection gave %v and %d sockets", err, r.dialCount())
	}
	r.acknowledge(true)

	conn, err := r.connect(farEnd, 5*time.Second)
	if err != nil {
		t.Fatalf("a connection through the Carrier: %v", err)
	}
	defer conn.Close()
	// The socket is bound to the Interface in use, by its index, and goes
	// to the address the sandbox named: no name, no other port.
	if got := r.dialsTo(farEnd.String()); len(got) == 0 || got[len(got)-1] != (dialed{20, "tcp4", farEnd.String()}) {
		t.Fatalf("the Carrier's sockets to the far end are %+v", got)
	}
	far := <-accepted
	defer far.Close()
	go far.Write([]byte("from the far end"))
	buf := make([]byte, 64)
	if n, err := io.ReadFull(conn, buf[:16]); err != nil || string(buf[:n]) != "from the far end" {
		t.Errorf("the sandbox read %q (%v)", buf[:n], err)
	}
	go conn.Write([]byte("from the sandbox"))
	if n, err := io.ReadFull(far, buf[:16]); err != nil || string(buf[:n]) != "from the sandbox" {
		t.Errorf("the far end read %q (%v)", buf[:n], err)
	}
	// The far end's end is the sandbox's.
	far.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err != io.EOF {
		t.Errorf("after the far end closed, the sandbox read %v", err)
	}
}

// The Carrier dials public addresses only: not a private one, not this
// Mac's own, not the sandbox's own way out. What it does not carry is
// silence, not a refusal.
func TestItCarriesOnlyToPublicAddresses(t *testing.T) {
	r := ready(t)
	r.serve()
	for what, dst := range map[string]string{
		"a private address":                  "10.0.0.5:22",
		"another private address":            "192.168.0.1:80",
		"this Mac's loopback":                "127.0.0.1:8080",
		"the link-local metadata address":    "169.254.169.254:80",
		"carrier-grade NAT":                  "100.64.0.1:80",
		"a stand-in address":                 "198.18.0.2:443",
		"this Mac's own address on its LAN":  "192.168.0.7:5000",
		"this Mac's own public address":      "198.51.100.7:5000",
		"the sandbox's expected exit":        exit.String() + ":443",
		"the sandbox's own address":          sandbox.SandboxAddress.String() + ":80",
		"multicast":                          "224.0.0.1:80",
		"the address of every host on a LAN": "255.255.255.255:80",
	} {
		_, err := r.connect(netip.MustParseAddrPort(dst), silence)
		if !heardNothing(err) {
			t.Errorf("%s (%s): the sandbox heard %v, want nothing", what, dst, err)
		}
		if got := r.dialsTo(dst); len(got) != 0 {
			t.Errorf("%s (%s): the Carrier made %d sockets", what, dst, len(got))
		}
	}
	// A public address beside them is carried.
	if conn, err := r.connect(farEnd, 5*time.Second); err != nil {
		t.Errorf("a public address: %v", err)
	} else {
		conn.Close()
	}
}

// Where an Allowed Interface answers names with stand-in addresses, the
// Carrier carries to those while it is the Interface in use: the
// sandbox's connections and the probe's alike. It carries to nothing
// else that is not public there, nor to the address this Mac itself has
// in that range, and to no stand-in address by another Interface.
func TestItCarriesToStandInAddressesOnlyWhereNamesAreAnsweredWithThem(t *testing.T) {
	r := newRigOf(t, sandbox.CarrierConfig{
		AllowedInterfaces:     []string{"utun9", "en0"},
		Resolver:              resolver.String(),
		ExpectedExitAddresses: []string{exit.String()},
		StandInInterfaces:     []string{"utun9"},
	}, tunnel(20), ethernet())
	r.look()
	r.acknowledge(true)
	r.serve()

	for _, dst := range []string{"198.18.0.13:443", "198.19.255.254:22"} {
		conn, err := r.connect(netip.MustParseAddrPort(dst), 5*time.Second)
		if err != nil {
			t.Fatalf("a stand-in address (%s) by the Interface that answers with them: %v", dst, err)
		}
		conn.Close()
		if got := r.dialsTo(dst); len(got) != 1 || got[0] != (dialed{20, "tcp4", dst}) {
			t.Errorf("a stand-in address (%s): the Carrier made the sockets %+v, want one bound to utun9", dst, got)
		}
	}
	if conn, err := r.probe(netip.MustParseAddrPort("198.18.0.14:443"), 5*time.Second); err != nil {
		t.Errorf("a probe's connection to a stand-in address: %v", err)
	} else {
		conn.Close()
	}
	for what, dst := range map[string]string{
		"this Mac's own address in the range": "198.18.0.1:9090",
		"a private address":                   "10.0.0.5:22",
		"another private address":             "192.168.0.1:80",
		"this Mac's loopback":                 "127.0.0.1:8080",
		"the link-local metadata address":     "169.254.169.254:80",
		"carrier-grade NAT":                   "100.64.0.1:80",
	} {
		if _, err := r.connect(netip.MustParseAddrPort(dst), silence); !heardNothing(err) {
			t.Errorf("%s (%s): the sandbox heard %v, want nothing", what, dst, err)
		}
		if got := r.dialsTo(dst); len(got) != 0 {
			t.Errorf("%s (%s): the Carrier made %d sockets", what, dst, len(got))
		}
	}

	// By the next Interface, which answers with none, they mean nothing.
	r.set(ethernet())
	r.acknowledge(true)
	const later = "198.18.0.15:443"
	if _, err := r.connect(netip.MustParseAddrPort(later), silence); !heardNothing(err) {
		t.Errorf("a stand-in address by an Interface that answers with none: the sandbox heard %v, want nothing", err)
	}
	if got := r.dialsTo(later); len(got) != 0 {
		t.Errorf("a stand-in address by an Interface that answers with none: the Carrier made %d sockets", len(got))
	}
	if conn, err := r.connect(farEnd, 5*time.Second); err != nil {
		t.Errorf("a public address by that Interface: %v", err)
	} else {
		conn.Close()
	}
}

// The sandbox hears a reset only when the far end refused. What fails
// on this Mac, and a far end that does not answer, are silence.
func TestAResetOnlyWhenTheFarEndRefused(t *testing.T) {
	r := ready(t)
	refuse := func(errno syscall.Errno) {
		r.mu.Lock()
		r.answer = func(context.Context, dialed) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp4", Err: os.NewSyscallError("connect", errno)}
		}
		r.mu.Unlock()
	}
	refuse(syscall.ECONNREFUSED)
	began := time.Now()
	if _, err := r.connect(farEnd, 5*time.Second); err == nil || heardNothing(err) {
		t.Errorf("a far end that refused: the sandbox heard %v, want a refusal", err)
	} else if took := time.Since(began); took > 2*time.Second {
		t.Errorf("a refusal took %s to reach the sandbox", took)
	}
	for what, errno := range map[string]syscall.Errno{
		"no route on this Mac":          syscall.ENETUNREACH,
		"no such host on this Mac":      syscall.EHOSTUNREACH,
		"an interface that is gone":     syscall.ENXIO,
		"a far end that did not answer": syscall.ETIMEDOUT,
		"the Carrier Wall":              syscall.EPERM,
	} {
		refuse(errno)
		if _, err := r.connect(farEnd, silence); !heardNothing(err) {
			t.Errorf("%s: the sandbox heard %v, want nothing", what, err)
		}
	}
	// And none of them moved the sandbox to another Interface.
	for _, d := range r.dialsTo(farEnd.String()) {
		if d.index != 20 {
			t.Errorf("after a dial failed, a socket was bound to interface %d", d.index)
		}
	}
	if len(r.reports) != 1 {
		t.Errorf("dials that failed changed the Interface in use: %+v", r.reports)
	}
}

// timedOut reports whether a read ended because its time was up: the
// connection is still there.
func timedOut(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

// expectReset waits for a connection of the sandbox's to be reset.
func expectReset(t *testing.T, conn net.Conn, why string) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Read(make([]byte, 16))
	if err == nil || err == io.EOF || timedOut(err) {
		t.Errorf("%s: the sandbox's connection read %v, want a reset", why, err)
	}
}

// Which Interface is in use is read from facts alone, and a change of it
// waits for the gatekeeper: nothing crosses the new Interface under the
// lease that was kept for the old one.
func TestAChangeOfInterface(t *testing.T) {
	r := ready(t)
	accepted := r.serve()
	conn, err := r.connect(farEnd, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	far := <-accepted

	// The tunnel goes: every flow is reset, the change is reported, and
	// nothing is carried by the next Interface yet.
	r.set(ethernet())
	if got := r.lastReport(); got != (sandbox.CarrierReport{Interface: "en0", InterfaceChange: 2}) {
		t.Fatalf("after the tunnel went the Carrier reports %+v", got)
	}
	expectReset(t, conn, "the Interface in use went")
	far.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := far.Read(make([]byte, 1)); err == nil {
		t.Error("the far end's side of a flow outlived the Interface it left by")
	}
	dials := r.dialCount()
	if _, err := r.connect(farEnd, silence); !heardNothing(err) || r.dialCount() != dials {
		t.Fatalf("before the gatekeeper acknowledged the change, a connection gave %v and %d sockets", err, r.dialCount()-dials)
	}
	// A judgement of the Interface before, however open, takes nothing
	// into use.
	r.c.judge(sandbox.CarrierJudgement{Open: true, Interface: "utun9", InterfaceChange: 1}, r.now)
	if _, err := r.connect(farEnd, silence); !heardNothing(err) || r.dialCount() != dials {
		t.Fatalf("a judgement of the Interface before let a connection through: %v", err)
	}
	// The gatekeeper names the change: the Interface is in use.
	r.acknowledge(true)
	second, err := r.connect(farEnd, 5*time.Second)
	if err != nil {
		t.Fatalf("after the change was acknowledged: %v", err)
	}
	if got := r.dialsTo(farEnd.String()); got[len(got)-1].index != 5 {
		t.Errorf("after the change the socket was bound to interface %d, want en0's", got[len(got)-1].index)
	}
	<-accepted

	// The preferred Interface returns: the switch back is at once, with
	// the same reset.
	r.set(ethernet(), tunnel(21))
	if got := r.lastReport(); got != (sandbox.CarrierReport{Interface: "utun9", InterfaceChange: 3}) {
		t.Fatalf("after the tunnel returned the Carrier reports %+v", got)
	}
	expectReset(t, second, "a more preferred Interface returned")
	r.acknowledge(true)
	third, err := r.connect(farEnd, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.dialsTo(farEnd.String()); got[len(got)-1].index != 21 {
		t.Errorf("after the tunnel returned the socket was bound to interface %d, want its new index", got[len(got)-1].index)
	}
	<-accepted

	// The tunnel is made again under its name: it is another interface.
	r.set(ethernet(), tunnel(22))
	if got := r.lastReport(); got != (sandbox.CarrierReport{Interface: "utun9", InterfaceChange: 4}) {
		t.Fatalf("after the tunnel was made again the Carrier reports %+v", got)
	}
	expectReset(t, third, "the Interface in use was made again under its name")

	// Facts that make an interface unusable: it is down, or has no address.
	down, bare := tunnel(22), tunnel(22)
	down.Up, bare.Addrs = false, nil
	for what, tun := range map[string]seenInterface{"down": down, "without an address": bare} {
		r.set(ethernet(), tunnel(22))
		r.set(ethernet(), tun)
		if got := r.lastReport(); got.Interface != "en0" {
			t.Errorf("with the tunnel %s the Interface in use is %q", what, got.Interface)
		}
	}
	// An interface that is not allowed is never used, whatever it is.
	other := seenInterface{Name: "en7", Index: 9, Up: true, Addrs: []netip.Addr{netip.MustParseAddr("192.168.9.9")}}
	r.set(other)
	if got := r.lastReport(); got.Interface != "" {
		t.Fatalf("with no Allowed Interface there, the Interface in use is %q", got.Interface)
	}
	// With none, nothing is carried, whatever the gatekeeper says.
	r.acknowledge(true)
	dials = r.dialCount()
	if _, err := r.connect(farEnd, silence); !heardNothing(err) || r.dialCount() != dials {
		t.Errorf("with no Interface in use a connection gave %v and %d sockets", err, r.dialCount()-dials)
	}
	// Looking again at what has not changed reports nothing.
	reports := len(r.reports)
	r.look()
	r.look()
	if len(r.reports) != reports {
		t.Errorf("the Carrier reported %d times for interfaces that did not change", len(r.reports)-reports)
	}
}

// With no word from the gatekeeper, the Carrier takes the Interface into
// use once the lease kept under the one before has run out, and no
// sooner.
func TestWithoutTheGatekeepersWordTheInterfaceWaitsOutTheLease(t *testing.T) {
	r := newRig(t, tunnel(20), ethernet())
	r.serve()
	r.look()
	for r.now.Sub(time.Unix(1_000_000, 0)) < sandbox.LeaseDuration+proceedMargin-lookEvery {
		r.look()
	}
	if _, err := r.probe(farEnd, silence); !heardNothing(err) || r.dialCount() != 0 {
		t.Fatalf("before the lease had run out, a probe's connection gave %v and %d sockets", err, r.dialCount())
	}
	r.look()
	r.look()
	r.look()
	// The Interface is in use, for the probe: nobody said the gate is
	// open, and nothing of the sandbox's own is carried.
	if conn, err := r.probe(farEnd, 5*time.Second); err != nil {
		t.Errorf("once the lease had run out, a probe's connection: %v", err)
	} else {
		conn.Close()
	}
	dials := r.dialCount()
	if _, err := r.connect(farEnd, silence); !heardNothing(err) || r.dialCount() != dials {
		t.Errorf("with no word of the gate, a connection of the sandbox's own gave %v and %d sockets", err, r.dialCount()-dials)
	}
}

// The Carrier keeps a connection only while it is told that the gate is
// open: bytes it holds for the sandbox do not leave after a lock.
func TestItKeepsAConnectionOnlyWhileTheGateIsOpen(t *testing.T) {
	r := ready(t)
	accepted := r.serve()
	open := func() net.Conn {
		t.Helper()
		conn, err := r.connect(farEnd, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		<-accepted
		return conn
	}

	// A judgement that the gate is locked resets every flow.
	conn := open()
	r.acknowledge(false)
	expectReset(t, conn, "the gate was judged locked")
	// While it is locked, a probe's connection is still carried: it is
	// how the gate opens.
	probe, err := r.probe(farEnd, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	<-accepted
	r.acknowledge(false)
	r.acknowledge(false)
	probe.SetReadDeadline(time.Now().Add(silence))
	if _, err := probe.Read(make([]byte, 1)); !timedOut(err) {
		t.Errorf("a connection made while the gate was locked was ended by the next judgement: %v", err)
	}
	probe.Close()

	// No judgement for as long as a lease lasts: the gate is not open.
	r.acknowledge(true)
	conn = open()
	for i := 0; i < int(sandbox.LeaseDuration/lookEvery)-1; i++ {
		r.look()
	}
	conn.SetReadDeadline(time.Now().Add(silence))
	if _, err := conn.Read(make([]byte, 1)); !timedOut(err) {
		t.Fatalf("a connection was ended before a lease's duration without a judgement: %v", err)
	}
	r.look()
	r.look()
	r.look()
	expectReset(t, conn, "nobody judged the gate for as long as a lease lasts")

	// A judgement keeps it open for another lease's duration.
	r.acknowledge(true)
	conn = open()
	for i := 0; i < 3*int(sandbox.LeaseDuration/lookEvery); i++ {
		r.look()
		if i%10 == 0 {
			r.acknowledge(true)
		}
	}
	conn.SetReadDeadline(time.Now().Add(silence))
	if _, err := conn.Read(make([]byte, 1)); !timedOut(err) {
		t.Errorf("a connection was ended while the gate was judged open: %v", err)
	}
}

// carries reports whether the far end reads what the sandbox writes on a
// connection, within the time the tests wait to say that nothing came.
func carries(conn, far net.Conn, what string) bool {
	go conn.Write([]byte(what))
	far.SetReadDeadline(time.Now().Add(silence))
	buf := make([]byte, len(what))
	_, err := io.ReadFull(far, buf)
	return err == nil && string(buf) == what
}

// arrives waits for the far end to read exactly what is expected.
func arrives(t *testing.T, far net.Conn, want, when string) {
	t.Helper()
	far.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(far, buf); err != nil || string(buf) != want {
		t.Fatalf("%s the far end read %q (%v), want %q", when, buf, err, want)
	}
}

// ends waits for the far end to read the end of what the sandbox sends,
// and nothing before it.
func ends(t *testing.T, far net.Conn, when string) {
	t.Helper()
	far.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := far.Read(make([]byte, 64)); n != 0 || err != io.EOF {
		t.Fatalf("%s the far end read %d bytes (%v), want the end of what the sandbox sends", when, n, err)
	}
}

// Under a Hold the Carrier keeps the connections it has and sends on
// nothing the sandbox sends on them, the end of what it sends no more
// than its bytes: that waits, in the order it came, and leaves once the
// gate is judged open again. What the far end sends still arrives. A
// probe's connection, known by its source port, waits for nothing.
func TestUnderAHoldWhatTheSandboxSendsWaits(t *testing.T) {
	r := ready(t)
	accepted := r.serve()
	open := func() (net.Conn, net.Conn) {
		t.Helper()
		conn, err := r.connect(farEnd, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return conn, <-accepted
	}

	conn, far := open()
	if !carries(conn, far, "open") {
		t.Fatal("under an open gate the connection carried nothing")
	}
	// A second connection, on which the sandbox sends nothing but its end.
	quiet, quietFar := open()
	r.hold()
	if carries(conn, far, "held, ") {
		t.Fatal("under a Hold, what the sandbox sent reached the far end")
	}
	// The far end's bytes still arrive.
	go far.Write([]byte("down"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if buf := make([]byte, 4); func() bool { _, err := io.ReadFull(conn, buf); return err != nil || string(buf) != "down" }() {
		t.Fatal("under a Hold, what the far end sent did not reach the sandbox")
	}
	// More is sent, and the sandbox ends its side: nothing leaves, and
	// the next judgement that the Hold lasts changes nothing.
	conn.Write([]byte("then more"))
	conn.(interface{ CloseWrite() error }).CloseWrite()
	r.hold()
	far.SetReadDeadline(time.Now().Add(silence))
	if n, err := far.Read(make([]byte, 64)); !timedOut(err) {
		t.Fatalf("under a Hold the far end read %d bytes (%v), want nothing, and no end", n, err)
	}
	// An end with no bytes before it waits as they do: it is what the
	// sandbox sends, and a FIN leaves this Mac as a byte does.
	quiet.(interface{ CloseWrite() error }).CloseWrite()
	quietFar.SetReadDeadline(time.Now().Add(silence))
	if n, err := quietFar.Read(make([]byte, 64)); !timedOut(err) {
		t.Fatalf("under a Hold the far end read %d bytes (%v) of a connection the sandbox ended, want no end", n, err)
	}

	// A probe's connection is made under the Hold, and carried.
	probe, err := r.probe(farEnd, 5*time.Second)
	if err != nil {
		t.Fatalf("a probe's connection under a Hold: %v", err)
	}
	probeFar := <-accepted
	if !carries(probe, probeFar, "probe") {
		t.Error("under a Hold a probe's connection carried nothing")
	}
	// A connection of the sandbox's own is not: no socket is made for it.
	dials := r.dialCount()
	if _, err := r.connect(netip.MustParseAddrPort("198.51.100.21:443"), silence); !heardNothing(err) || r.dialCount() != dials {
		t.Errorf("under a Hold a connection of the sandbox's own gave %v and %d sockets", err, r.dialCount()-dials)
	}

	// Open again: what waited leaves, in the order it came, and after it
	// the end of what the sandbox sent.
	r.acknowledge(true)
	arrives(t, far, "held, then more", "once the gate was open again")
	ends(t, far, "after what had waited,")
	ends(t, quietFar, "once the gate was open again")
}

// What waited under a Hold never leaves when the gate is then judged
// locked with none: every flow is reset, the probe's too.
func TestWhatWaitedUnderAHoldIsDroppedByALock(t *testing.T) {
	r := ready(t)
	accepted := r.serve()
	conn, err := r.connect(farEnd, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	far := <-accepted
	r.hold()
	probe, err := r.probe(farEnd, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	<-accepted
	conn.Write([]byte("never"))
	time.Sleep(silence / 4)
	r.acknowledge(false)
	expectReset(t, conn, "the gate was judged locked with no Hold")
	expectReset(t, probe, "the gate was judged locked with no Hold")
	far.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := far.Read(make([]byte, 16)); n != 0 || err == nil {
		t.Errorf("after the lock the far end read %d bytes (%v) of what had waited", n, err)
	}
}

// The Carrier sends on what the sandbox sends only for as long as the
// lease it was last told of runs: past that, with no judgement, it
// waits as under a Hold, and the next judgement that the gate is open
// lets it leave.
func TestWhatTheSandboxSendsWaitsOnceTheLeaseItWasToldOfIsOver(t *testing.T) {
	r := ready(t)
	accepted := r.serve()
	conn, err := r.connect(farEnd, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	far := <-accepted
	judge := func(leaseFor time.Duration) {
		report := r.lastReport()
		r.c.judge(sandbox.CarrierJudgement{Open: true, LeaseFor: leaseFor, Interface: report.Interface, InterfaceChange: report.InterfaceChange}, r.now)
	}

	judge(2 * time.Second)
	r.pass(2*time.Second - time.Millisecond)
	if !carries(conn, far, "in time") {
		t.Fatal("while the lease it was told of ran, the Carrier sent nothing on")
	}
	r.pass(time.Millisecond)
	if carries(conn, far, "late") {
		t.Fatal("once the lease it was told of was over, the Carrier sent on what the sandbox sent")
	}
	// The connection is kept: the gate was judged less than a lease's
	// duration ago.
	conn.SetReadDeadline(time.Now().Add(silence))
	if _, err := conn.Read(make([]byte, 1)); !timedOut(err) {
		t.Fatalf("the connection was ended when the lease the Carrier was told of was over: %v", err)
	}
	judge(time.Second)
	arrives(t, far, "late", "once the gate was judged open again")

	// An open gate with no lease to run lets nothing leave.
	judge(0)
	if carries(conn, far, "none") {
		t.Error("a gate judged open with no lease to run let the sandbox's bytes leave")
	}
}

// A Hold keeps nothing that the gate's other judgements end: a Hold of
// another Interface in use, a Hold nobody judged for as long as a lease
// lasts, and a change of the Interface in use.
func TestWhatEndsTheConnectionsUnderAHold(t *testing.T) {
	r := ready(t)
	accepted := r.serve()
	open := func() net.Conn {
		t.Helper()
		conn, err := r.connect(farEnd, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		<-accepted
		return conn
	}

	conn := open()
	r.c.judge(sandbox.CarrierJudgement{Held: true, Interface: "en0", InterfaceChange: 7}, r.now)
	expectReset(t, conn, "the Hold was of another Interface in use")

	// Nobody judges for as long as a lease lasts.
	r.acknowledge(true)
	conn = open()
	r.hold()
	for i := 0; i < int(sandbox.LeaseDuration/lookEvery)+3; i++ {
		r.look()
	}
	expectReset(t, conn, "nobody judged the gate for as long as a lease lasts, under a Hold")

	// The Interface in use changes under a Hold.
	r.acknowledge(true)
	conn = open()
	r.hold()
	r.set(ethernet())
	expectReset(t, conn, "the Interface in use changed under a Hold")
}

// A sandbox may hold only so many flows at once. Beyond them a new
// connection hears nothing.
func TestFlowsAreCapped(t *testing.T) {
	r := ready(t)
	r.c.maxFlows = 8
	accepted := r.serve()
	var held, far []net.Conn
	for i := 0; i < 8; i++ {
		conn, err := r.connect(farEnd, 5*time.Second)
		if err != nil {
			t.Fatalf("connection %d of 8: %v", i+1, err)
		}
		held = append(held, conn)
		far = append(far, <-accepted)
	}
	if _, err := r.connect(farEnd, silence); !heardNothing(err) {
		t.Errorf("a ninth connection heard %v, want nothing", err)
	}
	// A flow that ends, at both its ends, makes room for another.
	held[0].Close()
	far[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := r.connect(farEnd, silence)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a flow ended there was no room for another: %v", err)
		}
	}
	// Flows that are being made count too: a far end that does not answer
	// does not let a sandbox hold more.
	r.mu.Lock()
	r.answer = func(ctx context.Context, d dialed) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r.mu.Unlock()
	before := r.dialCount()
	for i := 0; i < 20; i++ {
		go r.connect(netip.AddrPortFrom(farEnd.Addr(), uint16(1000+i)), 2*silence)
	}
	time.Sleep(silence)
	if made := r.dialCount() - before; made > 8 {
		t.Errorf("with 8 flows allowed, %d sockets were being made at once", made)
	}
}

// A name query is UDP to the sandbox's resolver, port 53, and is carried
// as a connection is: bound, to that one address.
func TestANameQueryIsCarriedToTheResolver(t *testing.T) {
	r := ready(t)
	r.mu.Lock()
	r.answer = func(ctx context.Context, d dialed) (net.Conn, error) {
		near, far := net.Pipe()
		go func() {
			buf := make([]byte, 512)
			n, err := far.Read(buf)
			if err == nil {
				far.Write(append([]byte("answer to "), buf[:n]...))
			}
		}()
		return near, nil
	}
	r.mu.Unlock()
	query := func(to netip.AddrPort) (string, error) {
		conn, err := gonet.DialUDP(r.sandbox, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(to.Addr().As4()), Port: to.Port()}, ipv4.ProtocolNumber)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		conn.Write([]byte("a query"))
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 512)
		n, err := conn.Read(buf)
		return string(buf[:n]), err
	}
	to := netip.AddrPortFrom(resolver, 53)
	if got, err := query(to); err != nil || got != "answer to a query" {
		t.Fatalf("a name query came back as %q (%v)", got, err)
	}
	if got := r.dialsTo(to.String()); len(got) != 1 || got[0] != (dialed{20, "udp4", to.String()}) {
		t.Errorf("the name query's sockets are %+v", got)
	}
	// Other UDP is not carried, to the resolver or to anyone.
	before := r.dialCount()
	for _, other := range []string{"1.1.1.1:443", "8.8.8.8:53", "198.51.100.20:53"} {
		if got, err := query(netip.MustParseAddrPort(other)); err == nil {
			t.Errorf("UDP to %s was answered: %q", other, got)
		}
	}
	if r.dialCount() != before {
		t.Errorf("UDP that is no name query to the resolver made %d sockets", r.dialCount()-before)
	}

	// Under a Hold a name query of the sandbox's own is not sent on, and
	// the probe's, from one of its source ports, is.
	r.hold()
	if got, err := query(to); err == nil || r.dialCount() != before {
		t.Errorf("under a Hold a name query was answered %q, and made %d sockets", got, r.dialCount()-before)
	}
	conn, err := gonet.DialUDP(r.sandbox, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(sandbox.SandboxAddress.As4()), Port: sandbox.ProbeSourcePortFirst}, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(to.Addr().As4()), Port: to.Port()}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("a query"))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	if n, err := conn.Read(buf); err != nil || string(buf[:n]) != "answer to a query" {
		t.Errorf("under a Hold the probe's name query came back as %q (%v)", buf[:n], err)
	}
}

// The configuration a Carrier is given is checked before anything is
// carried.
func TestTheConfigurationIsChecked(t *testing.T) {
	good := sandbox.CarrierConfig{AllowedInterfaces: []string{"en0"}, Resolver: "1.1.1.1", ExpectedExitAddresses: []string{"203.0.113.9"}}
	if _, err := newCarrier(good); err != nil {
		t.Fatal(err)
	}
	for what, change := range map[string]func(*sandbox.CarrierConfig){
		"no Interface":                func(c *sandbox.CarrierConfig) { c.AllowedInterfaces = nil },
		"a resolver that is a name":   func(c *sandbox.CarrierConfig) { c.Resolver = "dns.example.com" },
		"a private resolver":          func(c *sandbox.CarrierConfig) { c.Resolver = "192.168.0.1" },
		"an exit address that is not": func(c *sandbox.CarrierConfig) { c.ExpectedExitAddresses = []string{"example.com"} },
		"stand-in addresses at an Interface that is not allowed": func(c *sandbox.CarrierConfig) {
			c.StandInInterfaces = []string{"utun4"}
		},
	} {
		cfg := good
		change(&cfg)
		if _, err := newCarrier(cfg); err == nil {
			t.Errorf("a configuration with %s was taken", what)
		}
	}
}

// The real look and the real dial, on this Mac: loopback is up and has
// an address, and a socket bound to it leads to no public address.
func TestTheRealLookAndTheBoundDial(t *testing.T) {
	seen := lookAtInterfaces()
	var lo seenInterface
	for _, s := range seen {
		if s.Name == "lo0" {
			lo = s
		}
	}
	if !lo.Up || len(lo.Addrs) == 0 || lo.Addrs[0] != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("this Mac's loopback is seen as %+v", lo)
	}
	if got := choose([]string{"nothere0", "lo0"}, seen); got != (chosen{"lo0", lo.Index}) {
		t.Errorf("of an interface that is not there and loopback, %+v is chosen", got)
	}
	if !ownAddresses(seen)[netip.MustParseAddr("127.0.0.1")] {
		t.Error("127.0.0.1 is not one of this Mac's own addresses")
	}
	// Bound to loopback, a public address is unreachable at once: nothing
	// leaves this Mac, by loopback or by another way.
	began := time.Now()
	conn, err := dialBound(context.Background(), lo.Index, "tcp4", "198.51.100.20:443")
	if err == nil {
		conn.Close()
		t.Fatal("a socket bound to loopback reached a public address")
	}
	if !errors.Is(err, syscall.ENETUNREACH) && !errors.Is(err, syscall.EHOSTUNREACH) || time.Since(began) > 2*time.Second {
		t.Errorf("a socket bound to loopback failed with %v after %s", err, time.Since(began))
	}
	// Bound to an interface that is not there, no socket is made.
	if conn, err := dialBound(context.Background(), 987654, "tcp4", "198.51.100.20:443"); err == nil {
		conn.Close()
		t.Error("a socket bound to no interface was made")
	}
}

// The Carrier makes every socket in one function, bound to an interface;
// it never dials unbound, never listens, and never resolves a name.
func TestEverySocketIsBoundAndNothingListens(t *testing.T) {
	// Calls that would make a socket another way, take a connection, or
	// resolve a name.
	forbidden := map[string]bool{
		"Dial": true, "DialTimeout": true, "DialTCP": true, "DialUDP": true, "DialIP": true, "DialUnix": true,
		"Listen": true, "ListenTCP": true, "ListenUDP": true, "ListenPacket": true, "ListenIP": true, "ListenUnix": true, "ListenConfig": true,
		"FileListener": true, "FilePacketConn": true, "Accept": true,
		"LookupHost": true, "LookupIP": true, "LookupAddr": true, "LookupCNAME": true, "LookupTXT": true, "LookupSRV": true, "LookupMX": true, "LookupNS": true, "LookupNetIP": true, "LookupIPAddr": true, "Resolver": true,
	}
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files, dialers, bound, viaField, selectors := 0, 0, 0, 0, 0
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
			case "net/http", "os/exec", "net/rpc", "net/smtp", "crypto/tls":
				t.Errorf("%s imports %s", name, path)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				selectors++
				pkg, _ := node.X.(*ast.Ident)
				if pkg != nil && pkg.Name == "net" && forbidden[node.Sel.Name] {
					t.Errorf("%s uses net.%s: the Carrier makes sockets in one function, bound, and takes none", name, node.Sel.Name)
				}
				if pkg != nil && pkg.Name == "net" && node.Sel.Name == "Dialer" {
					dialers++
					if name != "dial.go" {
						t.Errorf("%s makes a net.Dialer: only dial.go does", name)
					}
				}
				if node.Sel.Name == "IP_BOUND_IF" {
					bound++
				}
			case *ast.CallExpr:
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "dialBound" {
					viaField++
					if name != "dial.go" {
						t.Errorf("%s asks for a socket: only connect, in dial.go, does", name)
					}
				}
				// The gVisor stack's own listeners and dials are the
				// sandbox's side, not this Mac's: only the forwarders are used.
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
					if pkg, _ := sel.X.(*ast.Ident); pkg != nil && pkg.Name == "gonet" && (strings.HasPrefix(sel.Sel.Name, "Dial") || strings.HasPrefix(sel.Sel.Name, "Listen")) {
						t.Errorf("%s uses gonet.%s", name, sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	if files < 5 || selectors < 200 {
		t.Fatalf("read %d files with %d names: the test would prove little", files, selectors)
	}
	if dialers != 1 || bound != 1 || viaField != 1 {
		t.Errorf("the Carrier has %d dialers, binds in %d places and asks for a socket in %d: want one of each", dialers, bound, viaField)
	}
}

// The Carrier Wall: what the profile allows, line by line. A change to
// it is a change to what a Carrier can do of this Mac.
func TestTheCarrierWall(t *testing.T) {
	data, err := os.ReadFile("carrier.sb")
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, ";") {
			rules = append(rules, line)
		}
	}
	want := []string{
		`(version 1)`,
		`(deny default)`,
		`(allow process-exec (literal (param "CARRIER")))`,
		`(allow file-read* (literal (param "CARRIER")))`,
		`(allow file-read* (literal "/") (subpath "/usr/lib") (subpath "/System"))`,
		`(allow file-read-metadata)`,
		`(allow sysctl-read)`,
		`(allow network-outbound (remote ip "*:*"))`,
		`(deny network-outbound (remote ip "localhost:*"))`,
	}
	if strings.Join(rules, "\n") != strings.Join(want, "\n") {
		t.Errorf("the Carrier Wall's rules are\n%s\nwant\n%s", strings.Join(rules, "\n"), strings.Join(want, "\n"))
	}
}
