//go:build darwin

package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"zeolite/internal/sandbox"
)

// What one sandbox may hold of the Carrier at once. Beyond them a new
// connection hears nothing, as one the Carrier refuses does.
const (
	// maxFlows is how many connections and name queries are carried, or
	// being made, at once.
	maxFlows = 1024
	// maxQueued is how many packets wait for the Packet stream.
	maxQueued = 4096
	// bufferSize and maxBufferSize are how much one connection holds
	// each way: at first, and at most.
	bufferSize    = 1 << 20
	maxBufferSize = 4 << 20
	// queryIdle is how long a name query's socket waits for more.
	queryIdle = 10 * time.Second
)

// lookEvery is how often the Carrier looks at this Mac's interfaces, and
// proceedMargin how long past the lease's duration it waits for the
// gatekeeper to acknowledge a change before it takes the Interface into
// use all the same: the lease kept under the one before has run out by
// then, whatever became of the gatekeeper's word.
const (
	lookEvery     = 100 * time.Millisecond
	proceedMargin = time.Second
)

// flow is one connection or one name query's socket that the Carrier
// carries or is making.
type flow struct {
	ctx    context.Context
	cancel context.CancelFunc
	// probes is whether the flow is a probe's: it comes from one of the
	// Probe Source Ports, which the ruleset keeps for the gatekeeper. A
	// probe's flow waits for no lease: it is how the gate opens.
	probes bool
	// reset ends the flow at once, with a reset and not an orderly end.
	// It is set once the flow's two ends are there.
	mu    sync.Mutex
	reset func()
}

func (f *flow) end() {
	f.cancel()
	f.mu.Lock()
	reset := f.reset
	f.mu.Unlock()
	if reset != nil {
		reset()
	}
}

// carrier carries one sandbox's traffic: it ends the sandbox's
// connections in a network stack of its own and makes each again on a
// socket bound to the Interface in use.
type carrier struct {
	allowed  []string
	resolver netip.Addr
	// exits are the addresses the sandbox is expected to be seen at.
	exits map[netip.Addr]bool
	// standIns names the Allowed Interfaces where names are answered
	// with stand-in addresses: those are carried to by such an Interface
	// in use, and by no other.
	standIns map[string]bool
	// maxFlows is how many flows the sandbox may have at once.
	maxFlows int

	// look reads this Mac's interfaces, dialBound makes a bound socket,
	// and report tells the app the Interface in use. Tests put their own
	// in their place.
	look      func() []seenInterface
	dialBound func(ctx context.Context, index int, network, address string) (net.Conn, error)
	report    func(sandbox.CarrierReport)

	mu sync.Mutex
	// first is the first Allowed Interface that can be used, as last
	// looked at, change the count of its changes, and changed when it
	// last changed.
	first   chosen
	change  uint64
	changed time.Time
	// inUse is the Interface in use: first, once the gatekeeper has
	// acknowledged the change that made it so, and nothing until then.
	inUse chosen
	// open is whether the gate was last judged open, under this
	// Interface in use, and openUntil until when the lease it was judged
	// open by runs, by this Mac's clock; held whether it was judged
	// locked with a Hold that lasts, and judged when.
	open      bool
	openUntil time.Time
	held      bool
	judged    time.Time
	// told is closed when the gate is judged again: what waits for an
	// open gate then looks once more.
	told chan struct{}
	now  func() time.Time
	// own are this Mac's own addresses, as last looked at.
	own   map[netip.Addr]bool
	flows map[*flow]struct{}

	// What was carried, and what was not.
	carriedFlows, refusedFlows, droppedPackets atomic.Int64
}

func newCarrier(cfg sandbox.CarrierConfig) (*carrier, error) {
	resolver, err := sandbox.ParsePublicIPv4(cfg.Resolver)
	if err != nil {
		return nil, errors.New("the resolver is not a public IPv4 address")
	}
	if len(cfg.AllowedInterfaces) == 0 {
		return nil, errors.New("no Interface is allowed")
	}
	c := &carrier{
		allowed:   cfg.AllowedInterfaces,
		resolver:  resolver,
		exits:     map[netip.Addr]bool{},
		standIns:  map[string]bool{},
		maxFlows:  maxFlows,
		look:      lookAtInterfaces,
		dialBound: dialBound,
		report:    func(sandbox.CarrierReport) {},
		flows:     map[*flow]struct{}{},
		told:      make(chan struct{}),
		now:       time.Now,
	}
	for _, exit := range cfg.ExpectedExitAddresses {
		addr, err := sandbox.ParsePublicIPv4(exit)
		if err != nil {
			return nil, errors.New("an expected exit address is not a public IPv4 address")
		}
		c.exits[addr] = true
	}
	for _, name := range cfg.StandInInterfaces {
		if !slices.Contains(cfg.AllowedInterfaces, name) {
			return nil, errors.New("an Interface that is not allowed is said to answer names with stand-in addresses")
		}
		c.standIns[name] = true
	}
	return c, nil
}

// takeFlows empties the set of flows and returns what it held. The
// caller ends them, once it holds no lock.
func (c *carrier) takeFlows() []*flow {
	taken := make([]*flow, 0, len(c.flows))
	for f := range c.flows {
		taken = append(taken, f)
	}
	c.flows = map[*flow]struct{}{}
	return taken
}

func endAll(flows []*flow) {
	for _, f := range flows {
		f.end()
	}
}

// lookNow looks at this Mac's interfaces once. When the first usable
// Allowed Interface is another than it was, every flow is reset, pending
// dials are cancelled, nothing is carried, and the change is reported:
// the Interface is taken into use only when the gatekeeper has ended the
// lease kept under the one before.
func (c *carrier) lookNow(now time.Time) {
	seen := c.look()
	first := choose(c.allowed, seen)
	c.mu.Lock()
	c.own = ownAddresses(seen)
	var ended []*flow
	var report *sandbox.CarrierReport
	switch {
	case first != c.first:
		c.first, c.inUse, c.changed = first, chosen{}, now
		c.change++
		c.open, c.held, c.openUntil = false, false, time.Time{}
		ended = c.takeFlows()
		report = &sandbox.CarrierReport{Interface: first.name, InterfaceChange: c.change}
	case c.inUse != c.first && now.Sub(c.changed) > sandbox.LeaseDuration+proceedMargin:
		// No word of the gatekeeper's: the old lease has run out by now.
		c.inUse = c.first
	}
	// A gate nobody has judged for as long as a lease lasts is not open,
	// and holds nothing: what the Carrier holds for the sandbox must not
	// leave.
	if (c.open || c.held) && now.Sub(c.judged) > sandbox.LeaseDuration {
		c.open, c.held, c.openUntil = false, false, time.Time{}
		ended = append(ended, c.takeFlows()...)
	}
	c.mu.Unlock()
	endAll(ended)
	if report != nil {
		c.report(*report)
	}
}

// watch looks at this Mac's interfaces until ctx ends.
func (c *carrier) watch(ctx context.Context) {
	ticker := time.NewTicker(lookEvery)
	defer ticker.Stop()
	for {
		c.lookNow(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// judge takes what the gatekeeper last said. A judgement that names the
// change the Carrier reported takes the Interface into use. A gate that
// is neither open nor held ends every flow: bytes the Carrier already
// holds would otherwise leave after the lock. One that is locked with a
// Hold that lasts ends none: the flows are kept, and what the sandbox
// sends on them waits, until a judgement says the gate is open again or
// says neither.
//
// An open gate is open for as long as its lease had to run when the
// gatekeeper said so, counted from now: past that, with no judgement
// since, the Carrier knows of no lease.
func (c *carrier) judge(j sandbox.CarrierJudgement, now time.Time) {
	c.mu.Lock()
	current := j.InterfaceChange == c.change && j.Interface == c.first.name
	if current && c.first != (chosen{}) {
		c.inUse = c.first
	}
	was := c.open || c.held
	c.open, c.openUntil = j.Open && current, time.Time{}
	if c.open {
		c.openUntil = now.Add(j.LeaseFor)
	}
	c.held = j.Held && current && !c.open
	c.judged = now
	var ended []*flow
	if was && !c.open && !c.held {
		ended = c.takeFlows()
	}
	close(c.told)
	c.told = make(chan struct{})
	c.mu.Unlock()
	endAll(ended)
}

// leaves is whether what the sandbox sends on a flow may leave this Mac
// now: the flow is a probe's, or the gate was judged open by a lease
// that still runs. Nothing of the sandbox's own leaves otherwise,
// whatever the ruleset let through to the Carrier: under a Hold that is
// what belongs to the connections the sandbox has.
func (c *carrier) leaves(f *flow) bool {
	leaves, _ := c.leavesOrTold(f)
	return leaves
}

// leavesOrTold is leaves, and with it what is closed when the gate is
// next judged.
func (c *carrier) leavesOrTold(f *flow) (bool, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return f.probes || c.open && c.now().Before(c.openUntil), c.told
}

// proceed waits until what the sandbox sends on a flow may leave: for
// the next judgement that says the gate is open. It fails when ctx ends
// first, as it does when the flow is ended.
func (c *carrier) proceed(ctx context.Context, f *flow) error {
	for {
		leaves, told := c.leavesOrTold(f)
		if leaves {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-told:
		}
	}
}

// admit makes room for one more flow, from a port of the sandbox's
// namespace, or says that there is none.
func (c *carrier) admit(from uint16) (*flow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.flows) >= c.maxFlows {
		return nil, false
	}
	f := &flow{probes: sandbox.ProbeSourcePort(from)}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	c.flows[f] = struct{}{}
	return f, true
}

func (c *carrier) forget(f *flow) {
	f.cancel()
	c.mu.Lock()
	delete(c.flows, f)
	c.mu.Unlock()
}

// relay copies one way until the source ends or either side fails.
// before, where there is one, is asked before each write, and may wait.
func relay(dst io.Writer, src io.Reader, before func() error) error {
	buf := make([]byte, 128<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if before != nil {
				if berr := before(); berr != nil {
					return berr
				}
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// handleTCP carries one connection of the sandbox's. The sandbox hears a
// reset only when the far end refused the connection. What the Carrier
// does not carry, and what fails on this Mac, is silence.
//
// A connection of the sandbox's own is made, and what the sandbox sends
// on it is sent on, only while the Carrier knows of a lease: what comes
// meanwhile waits here, and what the far end sends still arrives.
func (c *carrier) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := netip.AddrPortFrom(netip.AddrFrom4(id.LocalAddress.As4()), id.LocalPort)
	f, ok := c.admit(id.RemotePort)
	if !ok {
		c.refusedFlows.Add(1)
		r.Complete(false)
		return
	}
	defer c.forget(f)
	if !c.awaitLease(f) {
		c.refusedFlows.Add(1)
		r.Complete(false)
		return
	}
	out, err := c.connect(f.ctx, "tcp4", dst)
	if err != nil {
		c.refusedFlows.Add(1)
		r.Complete(errors.Is(err, syscall.ECONNREFUSED))
		return
	}
	defer out.Close()
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		r.Complete(false)
		return
	}
	r.Complete(false)
	in := gonet.NewTCPConn(&wq, ep)
	defer in.Close()
	var once sync.Once
	reset := func() {
		once.Do(func() {
			ep.Abort()
			if t, ok := out.(*net.TCPConn); ok {
				t.SetLinger(0)
			}
			out.Close()
		})
	}
	f.mu.Lock()
	f.reset = reset
	f.mu.Unlock()
	// The Interface in use may have changed, or the gate locked, while
	// the connection was made: it is then one of the flows that ended.
	if f.ctx.Err() != nil {
		reset()
		return
	}
	c.carriedFlows.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		leave := func() error { return c.proceed(f.ctx, f) }
		// The end of what the sandbox sends is sent on as its bytes are.
		if relay(out, in, leave) != nil || leave() != nil {
			reset()
			return
		}
		if t, ok := out.(interface{ CloseWrite() error }); ok {
			t.CloseWrite()
		}
	}()
	if relay(in, out, nil) != nil {
		reset()
	} else {
		in.CloseWrite()
	}
	<-done
}

// awaitLease waits, before a flow's socket is made, until what the
// sandbox sends on it may leave, and no longer than a connection may
// take to be made: a flow that comes a moment before the Carrier is
// told that the gate is open is made then. With no Interface in use
// there is nothing to wait for.
func (c *carrier) awaitLease(f *flow) bool {
	c.mu.Lock()
	none := c.inUse == (chosen{})
	c.mu.Unlock()
	if none {
		return false
	}
	ctx, cancel := context.WithTimeout(f.ctx, dialTimeout)
	defer cancel()
	return c.proceed(ctx, f) == nil
}

// handleUDP carries a name query: UDP to the sandbox's resolver, port
// 53, and nothing else.
func (c *carrier) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := netip.AddrPortFrom(netip.AddrFrom4(id.LocalAddress.As4()), id.LocalPort)
	if dst.Port() != 53 || dst.Addr() != c.resolver {
		c.refusedFlows.Add(1)
		return false
	}
	f, ok := c.admit(id.RemotePort)
	if !ok {
		c.refusedFlows.Add(1)
		return false
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		c.forget(f)
		return false
	}
	in := gonet.NewUDPConn(&wq, ep)
	go func() {
		defer c.forget(f)
		defer in.Close()
		if !c.awaitLease(f) {
			c.refusedFlows.Add(1)
			return
		}
		out, err := c.connect(f.ctx, "udp4", dst)
		if err != nil {
			c.refusedFlows.Add(1)
			return
		}
		defer out.Close()
		var once sync.Once
		reset := func() { once.Do(func() { out.Close(); in.Close() }) }
		f.mu.Lock()
		f.reset = reset
		f.mu.Unlock()
		if f.ctx.Err() != nil {
			return
		}
		c.carriedFlows.Add(1)
		go func() {
			buf := make([]byte, 65535)
			for {
				out.SetReadDeadline(time.Now().Add(queryIdle))
				n, err := out.Read(buf)
				if err != nil {
					reset()
					return
				}
				in.Write(buf[:n])
			}
		}()
		buf := make([]byte, 65535)
		for {
			in.SetReadDeadline(time.Now().Add(queryIdle))
			n, err := in.Read(buf)
			if err != nil {
				reset()
				return
			}
			// A query asked under no lease is not sent on: whoever asked
			// asks again.
			if c.leaves(f) {
				out.Write(buf[:n])
			}
		}
	}()
	return true
}

// carry moves the sandbox's packets between its Packet stream and the
// network stack that ends its connections, until the stream ends or ctx
// does. Then every flow is reset.
func (c *carrier) carry(ctx context.Context, fromSandbox io.Reader, toSandbox io.Writer) error {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	defer s.Close()
	link := channel.New(maxQueued, sandbox.PacketMTU, "")
	if err := s.CreateNIC(1, link); err != nil {
		return errors.New(err.String())
	}
	// The stack's one interface takes a packet whatever address it is
	// for, and answers from that address: it stands in for every host
	// the sandbox talks to.
	s.SetPromiscuousMode(1, true)
	s.SetSpoofing(1, true)
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	sack := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	moderate := tcpip.TCPModerateReceiveBufferOption(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &moderate)
	receive := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: bufferSize, Max: maxBufferSize}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &receive)
	send := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: bufferSize, Max: maxBufferSize}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &send)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcp.NewForwarder(s, 0, maxFlows, c.handleTCP).HandlePacket)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udp.NewForwarder(s, c.handleUDP).HandlePacket)

	ended := make(chan error, 2)
	// The Packet stream to the stack: only what the Carrier reads.
	go func() {
		r := bufio.NewReaderSize(fromSandbox, 1<<16)
		packet := make([]byte, sandbox.MaxPacket)
		for {
			n, err := sandbox.ReadPacket(r, packet)
			if err != nil {
				ended <- err
				return
			}
			if !carried(packet[:n], c.resolver) {
				c.droppedPackets.Add(1)
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet[:n])})
			link.InjectInbound(header.IPv4ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
	// The stack to the Packet stream. Packets that are waiting go out in
	// one write. It outlives ctx by a moment: the last resets still cross.
	pumping, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		w := bufio.NewWriterSize(toSandbox, 1<<16)
		var frame []byte
		for {
			pkt := link.ReadContext(pumping)
			if pkt == nil {
				return
			}
			view := pkt.ToView()
			pkt.DecRef()
			var err error
			frame, err = sandbox.AppendPacket(frame[:0], view.AsSlice())
			view.Release()
			if err != nil {
				continue
			}
			if _, err := w.Write(frame); err != nil {
				ended <- err
				return
			}
			if link.NumQueued() == 0 && w.Flush() != nil {
				ended <- io.ErrClosedPipe
				return
			}
		}
	}()

	var err error
	select {
	case <-ctx.Done():
	case err = <-ended:
	}
	// Nothing in the sandbox waits on a connection that nobody carries
	// any more.
	c.mu.Lock()
	open := c.takeFlows()
	c.mu.Unlock()
	endAll(open)
	time.Sleep(200 * time.Millisecond) // the resets cross the stream
	if err == io.EOF {
		err = nil
	}
	return err
}
