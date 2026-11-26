package orchestrator

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
	"zeolite/internal/sandboximage"
)

const (
	roleGatekeeper  = "gatekeeper"
	roleWorkstation = "workstation"

	// sandboxHostname is what every sandbox calls itself.
	sandboxHostname = "sandbox"

	// workstationPids bounds the processes of a workstation, so that a
	// runaway agent exhausts its own sandbox and not the VM.
	workstationPids = 4096

	readyTimeout    = 30 * time.Second
	timezoneTimeout = 30 * time.Second
	// gateOpenTimeout is how long a sandbox's gate has to open before
	// its workstation's image is built by way of it.
	gateOpenTimeout = 30 * time.Second
)

// pod is a running sandbox.
type pod struct {
	cfg         sandbox.Config
	gatekeeper  string // container ID
	workstation string // container ID
	// carrier carries the sandbox's traffic on the Mac, nil until the
	// gatekeeper started.
	carrier Carrier
	// sharedFolder is the Shared Folder the workstation mounts, as its
	// path on the Mac, empty for none.
	sharedFolder string
	// stop ends the pod's goroutines and its heartbeat stream.
	stop context.CancelFunc
	// beatNow asks for a heartbeat at once, without waiting for the next.
	beatNow chan struct{}
	// sessions are the IDs of the Terminal Sessions that are open in the
	// workstation: the ones the app opened there, and has not closed.
	sessions map[string]bool

	mu       sync.Mutex
	status   sandbox.Status
	stopping bool
	// inUse is the Interface in use as the sandbox's Carrier last
	// reported it: what the next heartbeat tells the gatekeeper.
	inUse sandbox.CarrierReport
	// gate is what the gatekeeper last said of the gate, and gateSaid is
	// closed when it says more. gone is closed when the gatekeeper is
	// heard from no more.
	gate     sandbox.GateTelemetry
	gateSaid chan struct{}
	gone     chan struct{}
}

// Carrier is a sandbox's Carrier as the orchestrator uses it: the
// program on the Mac that carries the sandbox's traffic.
type Carrier interface {
	// Judge passes on what the gatekeeper last said of the gate, and
	// which Interface in use it has taken note of.
	Judge(sandbox.CarrierJudgement)
	// Stop ends the Carrier, and with it every connection it holds.
	Stop()
}

// StartOptions is what a sandbox needs from the Mac while it runs.
type StartOptions struct {
	// ConditionsMet is asked, for every heartbeat, whether the sandbox's
	// Host Conditions hold.
	ConditionsMet func() bool
	// PhysicalExits is asked for this Mac's physical exit addresses, and
	// whether they are known, for every heartbeat while the Interface in
	// use expects those, or any exit address but those. beside is the
	// interface that is not counted among this Mac's physical Interfaces:
	// the Interface in use where any but physical is expected, and none
	// where physical is. It must not wait. Without it they are not known.
	PhysicalExits func(beside string) (addrs []string, known bool)
	// Carry starts the sandbox's Carrier on its Packet stream, whose
	// bytes the orchestrator hands over unread. report is told the
	// Interface in use each time it changes, and ended that the Carrier
	// is gone, which is the end of the sandbox. A sandbox with a network
	// does not start without it.
	Carry func(cfg sandbox.CarrierConfig, stream io.ReadWriteCloser, report func(sandbox.CarrierReport), ended func(error)) (Carrier, error)
	// SharedFolder is the Shared Folder the sandbox is given, as its path
	// on the Mac, empty for a sandbox that has none. The VM mounts the
	// root that holds it, and the workstation sees it at
	// sandbox.SharedFolderPath.
	SharedFolder string
	// Link serves the sandbox's Link on its stream, whose bytes the
	// orchestrator hands over unread, and returns when the stream ends.
	// Without it the sandbox has no Link: nothing in its workstation
	// then asks anything of the app.
	Link func(stream io.ReadWriteCloser)
}

// Start creates and starts a sandbox. It returns once the workstation
// runs; the gate's state follows in Updates.
func (o *Orchestrator) Start(ctx context.Context, cfg sandbox.Config, packs []pack.Manifest, opts StartOptions) (err error) {
	if errs := cfg.Validate(); len(errs) > 0 {
		return errs[0]
	}
	o.mu.Lock()
	if _, running := o.running[cfg.ID]; running {
		o.mu.Unlock()
		return fmt.Errorf("sandbox %s is already running", cfg.Name)
	}
	podCtx, stop := context.WithCancel(context.Background())
	p := &pod{cfg: cfg, stop: stop, sharedFolder: opts.SharedFolder, gateSaid: make(chan struct{}), gone: make(chan struct{})}
	o.running[cfg.ID] = p
	o.mu.Unlock()

	o.setStatus(p, sandbox.Status{State: sandbox.StateStarting})
	defer func() {
		if err != nil {
			o.teardown(context.WithoutCancel(ctx), p)
			o.setStatus(p, sandbox.Status{State: sandbox.StateError, Error: err.Error()})
		}
	}()

	// The backstop is checked first: it stands behind whatever is on a
	// bridge, which nothing here is asked to be.
	if err := o.verifyBackstop(ctx); err != nil {
		return err
	}
	workstationImage, err := sandboximage.Workstation(o.arch, packs)
	if err != nil {
		return err
	}
	// What an earlier run of this sandbox left, if anything.
	o.removeContainers(ctx, cfg.ID)

	gatekeeperImage, err := o.ensureGatekeeperImage(ctx)
	if err != nil {
		return err
	}
	timezone := cfg.Clock.Timezone
	tz, err := o.startGatekeeper(ctx, podCtx, p, gatekeeperImage.Tag, opts)
	if err != nil {
		return err
	}
	if cfg.Clock.Timezone == sandbox.TimezoneAuto {
		timezone = tz
	}

	// The workstation's image is built only now, where it is not there:
	// in the namespace of a gatekeeper whose ruleset was just verified,
	// so that its steps leave as the sandbox will, or not at all.
	if err := o.ensureWorkstationImage(ctx, podCtx, p, workstationImage); err != nil {
		return err
	}
	// The workstation is created against that gatekeeper, in the
	// namespace it joins.
	if err := o.startWorkstation(ctx, podCtx, p, workstationImage.Tag, timezone, packs); err != nil {
		return err
	}
	if opts.Link != nil {
		if err := o.openLink(ctx, podCtx, p, opts.Link); err != nil {
			return err
		}
	}
	return nil
}

// SharedFolderMounted reports whether a sandbox that is starting or
// running mounts the Shared Folder at this path on the Mac, or is about
// to.
func (o *Orchestrator) SharedFolderMounted(path string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range o.running {
		if p.sharedFolder != "" && p.sharedFolder == path {
			return true
		}
	}
	return false
}

// SetSuspended says that the Mac is about to sleep, or has woken. While
// it is suspended every gate is locked, at once: heartbeats say that the
// Host Conditions do not hold, whatever they are. After it wakes they
// are evaluated afresh.
func (o *Orchestrator) SetSuspended(suspended bool) {
	o.suspended.Store(suspended)
	o.BeatNow()
}

// BeatNow sends every running sandbox's gatekeeper a heartbeat at once,
// without waiting for the next: what a heartbeat says has changed.
func (o *Orchestrator) BeatNow() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range o.running {
		if p.beatNow != nil {
			select {
			case p.beatNow <- struct{}{}:
			default:
			}
		}
	}
}

// heartbeat is what a sandbox's gatekeeper is told now: whether its
// Host Conditions hold, the Interface in use as its Carrier last
// reported it, and, where that Interface expects this Mac's physical
// exit address or any but it, which those are.
func (o *Orchestrator) heartbeat(p *pod, opts StartOptions) sandbox.Heartbeat {
	// While the Mac is going to sleep nothing may pass: what it wakes up
	// to may be another network.
	met := !o.suspended.Load() && (opts.ConditionsMet == nil || opts.ConditionsMet())
	// A pair the gatekeeper has not seen ends the lease.
	p.mu.Lock()
	inUse := p.inUse
	p.mu.Unlock()
	h := sandbox.Heartbeat{HostConditionsMet: met, Interface: inUse.Interface, InterfaceChange: inUse.InterfaceChange}
	// This Mac's physical exit addresses are read only for a sandbox that
	// leaves by an Interface that expects them: for no other is a probe
	// sent from this Mac's physical Interfaces.
	if opts.PhysicalExits != nil && inUse.Interface != "" {
		for _, ifc := range p.cfg.AllowedInterfaces {
			if ifc.Name != inUse.Interface {
				continue
			}
			switch {
			case ifc.AnyExitAddressButPhysical:
				// The Interface in use is the way out that is expected
				// to lead elsewhere: it is not one of this Mac's
				// physical Interfaces.
				h.PhysicalExitAddresses, h.PhysicalExitKnown = opts.PhysicalExits(inUse.Interface)
			case ifc.PhysicalExitAddress:
				// Every one of this Mac's physical Interfaces counts,
				// the Interface in use among them where it is one.
				h.PhysicalExitAddresses, h.PhysicalExitKnown = opts.PhysicalExits("")
			}
		}
	}
	return h
}

// Stop removes a sandbox's containers and ends its Carrier. Every
// process goes with them, and /dev/shm, which is memory, and /tmp, a
// volume that Docker made with the workstation and removes with it. Its
// Home, which is on the VM's disk too, and its Shared Folder, which is
// on the Mac's, stay.
func (o *Orchestrator) Stop(ctx context.Context, sandboxID string) {
	o.mu.Lock()
	p := o.running[sandboxID]
	o.mu.Unlock()
	if p == nil {
		return
	}
	o.teardown(ctx, p)
	o.setStatus(p, sandbox.Status{State: sandbox.StateStopped})
}

func (o *Orchestrator) teardown(ctx context.Context, p *pod) {
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	p.stop()
	o.removeContainers(ctx, p.cfg.ID)
	// With the gatekeeper gone the Packet stream has ended, and the
	// Carrier with it. It is told all the same.
	if p.carrier != nil {
		p.carrier.Stop()
	}
	o.mu.Lock()
	delete(o.running, p.cfg.ID)
	o.mu.Unlock()
}

// removeContainers removes a sandbox's containers, the workstation
// first.
func (o *Orchestrator) removeContainers(ctx context.Context, sandboxID string) {
	for _, name := range []string{WorkstationContainer(sandboxID), GatekeeperContainer(sandboxID)} {
		o.removeContainer(ctx, name)
	}
}

// removeContainer removes one container of a sandbox, running or not,
// and the volumes Docker made for it alone: a workstation's /tmp. It is
// the one way a sandbox's container is removed. A volume that has a
// name is not removed with a container, whatever is asked: a Home stays.
func (o *Orchestrator) removeContainer(ctx context.Context, name string) error {
	_, err := o.docker.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
}

// setStatus records a pod's status and tells the listener when it changed.
func (o *Orchestrator) setStatus(p *pod, s sandbox.Status) {
	p.mu.Lock()
	changed := p.status != s
	p.status = s
	p.mu.Unlock()
	if changed {
		o.listen(sandbox.Update{SandboxID: p.cfg.ID, Status: &s})
	}
}

func labels(sandboxID, role string) map[string]string {
	return map[string]string{labelSandbox: sandboxID, labelRole: role}
}

// logConfig keeps a container's logging from ever blocking it: a
// gatekeeper that waited on a stalled log would stop judging its gate.
var logConfig = container.LogConfig{
	Type:   "json-file",
	Config: map[string]string{"mode": "non-blocking", "max-buffer-size": "1m", "max-size": "5m", "max-file": "1"},
}

// tunReady is how long tun has to make the sandbox's device.
const tunReady = 15 * time.Second

// startGatekeeper creates the sandbox's gatekeeper, gives its namespace
// the TUN device and starts the sandbox's Carrier on the Mac, in the
// order that leaves no moment at which the namespace has a device and
// no ruleset:
//
//  1. the gatekeeper starts with no network at all, and loads its ruleset;
//  2. the ruleset is verified, in a namespace that holds loopback alone;
//  3. tun makes the TUN device there, and its Packet stream is handed to
//     the Carrier;
//  4. the ruleset is verified again, with loopback and the device.
//
// A sandbox is on no Docker network: what it sends goes into the device,
// and from there to the Carrier or nowhere. It returns the timezone of
// the exit address when the Clock Policy asks for it.
func (o *Orchestrator) startGatekeeper(ctx, podCtx context.Context, p *pod, image string, opts StartOptions) (timezone string, err error) {
	cfg := p.cfg
	resolver, err := sandbox.ParsePublicIPv4(cfg.Gate.Resolver)
	if err != nil {
		return "", fmt.Errorf("the resolver %q: %w", cfg.Gate.Resolver, err)
	}
	if opts.Carry == nil {
		return "", errors.New("there is no Carrier to carry the sandbox's traffic")
	}

	pids := int64(256)
	created, err := o.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: GatekeeperContainer(cfg.ID),
		Config: &container.Config{
			Image: image,
			// The workstation shares these with the gatekeeper, as it
			// shares its network: its name, its hosts file and its
			// resolver file. None says anything of the Mac or the VM.
			Hostname:    sandboxHostname,
			Labels:      labels(cfg.ID, roleGatekeeper),
			OpenStdin:   true,
			StdinOnce:   true,
			AttachStdin: true,
		},
		HostConfig: &container.HostConfig{
			// No network of Docker's, now or later: the namespace gets
			// loopback, and the TUN device that tun makes in it.
			NetworkMode: "none",
			// Name queries go to the sandbox's resolver, from the
			// sandbox's own namespace and so through its ruleset. Naming
			// it keeps Docker from writing the VM's resolver here, and
			// the search domain keeps the VM's out too.
			DNS:       []netip.Addr{resolver},
			DNSSearch: []string{"."},
			// The sandbox's name for itself is its own address.
			ExtraHosts:     []string{sandboxHostname + ":" + sandbox.SandboxAddress.String()},
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{"NET_ADMIN"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			Tmpfs:          map[string]string{"/run": "rw,noexec,nosuid,size=1m"},
			// Docker must never restart it: a restarted gatekeeper has a
			// new namespace, and whatever joined it would be ungated
			// until the rules were loaded again.
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
			LogConfig:     logConfig,
			Resources: container.Resources{
				Memory:    256 << 20,
				PidsLimit: &pids,
				// The node tun makes the device through. The workstation
				// has none.
				Devices: []container.DeviceMapping{{PathOnHost: "/dev/net/tun", PathInContainer: "/dev/net/tun", CgroupPermissions: "rwm"}},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating the gatekeeper: %w", err)
	}
	p.gatekeeper = created.ID

	// The heartbeat stream is attached before the container starts, so
	// that its first line is the config.
	stream, err := o.docker.ContainerAttach(ctx, p.gatekeeper, client.ContainerAttachOptions{Stream: true, Stdin: true})
	if err != nil {
		return "", fmt.Errorf("attaching to the gatekeeper: %w", err)
	}
	go func() {
		<-podCtx.Done()
		stream.Close() // the end of the stream locks the gate for good
	}()
	if _, err := o.docker.ContainerStart(ctx, p.gatekeeper, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("starting the gatekeeper: %w", err)
	}

	gkConfig := sandbox.GatekeeperConfig{
		Resolver:          resolver.String(),
		AllowedInterfaces: cfg.AllowedInterfaces,
		ProbeURLs:         cfg.Gate.ProbeURLs,
		ProbePin:          cfg.Gate.ProbePin,
		Hold:              cfg.Gate.Hold,
	}
	if cfg.Clock.Timezone == sandbox.TimezoneAuto {
		gkConfig.TimezoneProbeURL = cfg.Clock.TimezoneProbeURL
	}
	line, _ := json.Marshal(gkConfig)
	if _, err := stream.Conn.Write(append(line, '\n')); err != nil {
		return "", fmt.Errorf("sending the gatekeeper its config: %w", err)
	}

	// From here on the gatekeeper reports, and expects heartbeats.
	ready := make(chan sandbox.GateReady, 4)
	failed := make(chan string, 1)
	go o.followGatekeeper(podCtx, p, ready, failed)
	p.beatNow = make(chan struct{}, 1)
	go func() {
		ticker := time.NewTicker(sandbox.HeartbeatInterval)
		defer ticker.Stop()
		for {
			beat, _ := json.Marshal(o.heartbeat(p, opts))
			if _, err := stream.Conn.Write(append(beat, '\n')); err != nil {
				return
			}
			select {
			case <-ticker.C:
			case <-p.beatNow:
			case <-podCtx.Done():
				return
			}
		}
	}()

	waitReady := func(timeout time.Duration, want func(sandbox.GateReady) bool) (sandbox.GateReady, error) {
		deadline := time.After(timeout)
		for {
			select {
			case r := <-ready:
				if want(r) {
					return r, nil
				}
			case msg := <-failed:
				return sandbox.GateReady{}, errors.New("the gatekeeper failed: " + msg)
			case <-deadline:
				return sandbox.GateReady{}, errors.New("the gatekeeper did not become ready")
			case <-ctx.Done():
				return sandbox.GateReady{}, ctx.Err()
			}
		}
	}
	first, err := waitReady(readyTimeout, func(sandbox.GateReady) bool { return true })
	if err != nil {
		return "", err
	}
	if err := o.verifyGatekeeper(ctx, p.gatekeeper, first.RulesetHash, "lo"); err != nil {
		return "", err
	}

	// The TUN device, made by tun in the gatekeeper's namespace. What
	// tun writes on standard output is the Packet stream; that the
	// device is there it says on standard error.
	said, saidLine := io.Pipe()
	packets, err := o.openExecStream(ctx, p.gatekeeper, "0", saidLine, sandboximage.TunPath)
	if err != nil {
		return "", fmt.Errorf("starting tun: %w", err)
	}
	made := make(chan string, 1)
	go func() {
		lines := bufio.NewScanner(said)
		first := true
		for lines.Scan() {
			if first {
				made <- lines.Text()
				first = false
			}
		}
	}()
	select {
	case line := <-made:
		if line != sandbox.TUNReady {
			packets.Close()
			return "", fmt.Errorf("the sandbox's TUN device was not made: %s", line)
		}
	case <-time.After(tunReady):
		packets.Close()
		return "", errors.New("the sandbox's TUN device was not made in time")
	case <-ctx.Done():
		packets.Close()
		return "", ctx.Err()
	}

	// The Carrier, on the Mac, takes the stream from here on.
	carrierConfig := sandbox.CarrierConfig{Resolver: resolver.String()}
	for _, ifc := range cfg.AllowedInterfaces {
		carrierConfig.AllowedInterfaces = append(carrierConfig.AllowedInterfaces, ifc.Name)
		carrierConfig.ExpectedExitAddresses = append(carrierConfig.ExpectedExitAddresses, ifc.ExpectedExitAddresses...)
		if ifc.StandInAddresses {
			carrierConfig.StandInInterfaces = append(carrierConfig.StandInInterfaces, ifc.Name)
		}
	}
	carrier, err := opts.Carry(carrierConfig, packets,
		func(report sandbox.CarrierReport) {
			p.mu.Lock()
			p.inUse = report
			p.mu.Unlock()
			// The gatekeeper is told at once.
			select {
			case p.beatNow <- struct{}{}:
			default:
			}
		},
		func(err error) { o.carrierEnded(p, err) })
	if err != nil {
		packets.Close()
		return "", fmt.Errorf("starting the sandbox's Carrier: %w", err)
	}
	p.mu.Lock()
	p.carrier = carrier
	p.mu.Unlock()

	if err := o.verifyGatekeeper(ctx, p.gatekeeper, first.RulesetHash, "lo", sandbox.TUNDevice); err != nil {
		return "", err
	}

	if gkConfig.TimezoneProbeURL != "" {
		r, err := waitReady(timezoneTimeout, func(r sandbox.GateReady) bool { return r.Timezone != "" })
		if err != nil {
			return "", fmt.Errorf("reading the timezone of the exit address: %w", err)
		}
		if _, err := time.LoadLocation(r.Timezone); err != nil || r.Timezone == "Local" {
			return "", fmt.Errorf("the timezone probe answered %q, which is not an IANA timezone", r.Timezone)
		}
		timezone = r.Timezone
	}
	return timezone, nil
}

// ensureWorkstationImage builds a workstation's image, unless it is
// there, in the namespace of the sandbox it is built for: its steps
// have loopback and the sandbox's TUN device, and what they send goes
// to the sandbox's Carrier, by the Interface in use and under the
// lease, as what the workstation sends will. They are refused while the
// gate is locked, so the gate is waited for first: a sandbox whose gate
// does not open builds no image, and one whose gate locks meanwhile
// loses the build, which the next start takes up again from the steps
// that were done.
//
// The image's base is another matter: Docker pulls it by itself, from
// the VM, by no Allowed Interface.
func (o *Orchestrator) ensureWorkstationImage(ctx, podCtx context.Context, p *pod, img sandboximage.Image) error {
	exists, err := o.imageExists(ctx, img.Tag)
	if err != nil || exists {
		return err
	}
	if err := awaitOpenGate(ctx, p, gateOpenTimeout); err != nil {
		return fmt.Errorf("the sandbox's image is built by way of its gate: %w", err)
	}
	// A build ends with its sandbox: its steps would have no way out.
	building, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(podCtx, cancel)()
	if err := o.buildImage(building, img, namespaceOf(p.gatekeeper)); err != nil {
		p.mu.Lock()
		gate := p.gate
		p.mu.Unlock()
		if !gate.Open {
			return fmt.Errorf("%w\nthe sandbox's gate, by way of which its image is built, is locked: %s", err, lockedFor(gate))
		}
		return err
	}
	return nil
}

// awaitOpenGate waits until a sandbox's gatekeeper says its gate is
// open, for as long as timeout, and says why it is locked when it does
// not open.
func awaitOpenGate(ctx context.Context, p *pod, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		gate, said := p.gate, p.gateSaid
		p.mu.Unlock()
		if gate.Open {
			return nil
		}
		select {
		case <-said:
		case <-p.gone:
			return errors.New("the gatekeeper stopped")
		case <-deadline.C:
			return fmt.Errorf("the gate did not open: %s", lockedFor(gate))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// lockedFor is why a gate is locked, in the words of its lock reason,
// with the address the probe saw where that is the reason.
func lockedFor(gate sandbox.GateTelemetry) string {
	switch {
	case gate.LockReason == "":
		return "the gatekeeper has not said why"
	case gate.LockReason == sandbox.LockExitAddressMismatch && gate.ExitAddress != "":
		return string(gate.LockReason) + ", seen at " + gate.ExitAddress
	}
	return string(gate.LockReason)
}

// carrierEnded is told that a sandbox's Carrier is gone by itself. The
// sandbox's Packet stream went with it, and so did its device: nothing
// of the sandbox can leave, and nothing is gained by keeping it.
func (o *Orchestrator) carrierEnded(p *pod, err error) {
	p.mu.Lock()
	stopping := p.stopping
	p.mu.Unlock()
	if stopping {
		return
	}
	why := "the sandbox's Carrier stopped"
	if err != nil {
		why += ": " + err.Error()
	}
	// A Carrier ends when its Packet stream does, which is what the end
	// of the gatekeeper looks like from the Mac: that is then what
	// happened.
	if inspected, inspectErr := o.docker.ContainerInspect(context.Background(), p.gatekeeper, client.ContainerInspectOptions{}); inspectErr != nil || inspected.Container.State == nil || !inspected.Container.State.Running {
		why = "the gatekeeper stopped"
	}
	o.teardown(context.Background(), p)
	o.setStatus(p, sandbox.Status{State: sandbox.StateError, Error: why})
}

// verifyGatekeeper asks, in the gatekeeper's namespace and now, whether
// its ruleset is live and intact, and whether the namespace holds the
// devices it should and no other.
func (o *Orchestrator) verifyGatekeeper(ctx context.Context, containerID, rulesetHash string, devices ...string) error {
	raw := make([]byte, 8)
	rand.Read(raw)
	nonce := hex.EncodeToString(raw)
	out, _, err := o.exec(ctx, containerID, "0", nil, sandboximage.GatekeeperPath, "verify", nonce)
	if err != nil {
		return fmt.Errorf("verifying the gate: %w", err)
	}
	var verdict struct {
		Nonce       string   `json:"nonce"`
		OK          bool     `json:"ok"`
		RulesetHash string   `json:"rulesetHash"`
		Devices     []string `json:"devices"`
		Problems    []string `json:"problems"`
	}
	if err := json.Unmarshal([]byte(out), &verdict); err != nil || verdict.Nonce != nonce {
		return fmt.Errorf("verifying the gate: unexpected answer %q", strings.TrimSpace(out))
	}
	if !verdict.OK || verdict.RulesetHash != rulesetHash {
		return fmt.Errorf("the gate is not in place: %s", strings.Join(verdict.Problems, "; "))
	}
	sort.Strings(verdict.Devices)
	sort.Strings(devices)
	if !slices.Equal(verdict.Devices, devices) {
		return fmt.Errorf("the sandbox's namespace holds the devices %v, and should hold %v", verdict.Devices, devices)
	}
	return nil
}

// followGatekeeper turns the gatekeeper's reports into Updates until the
// gatekeeper stops, which ends the sandbox.
func (o *Orchestrator) followGatekeeper(ctx context.Context, p *pod, ready chan<- sandbox.GateReady, failed chan<- string) {
	o.tail(ctx, p.gatekeeper, func(line []byte) {
		var r sandbox.GateReport
		if json.Unmarshal(line, &r) != nil {
			return
		}
		switch {
		case r.Ready != nil:
			select {
			case ready <- *r.Ready:
			default:
			}
		case r.Gate != nil:
			// What the gatekeeper says of the gate, and which Interface
			// in use it has taken note of, is the Carrier's to know.
			p.mu.Lock()
			carrier := p.carrier
			said := p.gateSaid
			p.gate, p.gateSaid = *r.Gate, make(chan struct{})
			p.mu.Unlock()
			close(said)
			if carrier != nil {
				carrier.Judge(sandbox.CarrierJudgement{Open: r.Gate.Open, LeaseFor: r.Gate.LeaseFor, Held: r.Gate.Held, Interface: r.Gate.Interface, InterfaceChange: r.Gate.InterfaceChange})
			}
			o.listen(sandbox.Update{SandboxID: p.cfg.ID, Gate: r.Gate})
			if p.workstation != "" {
				o.setStatus(p, gateStatus(*r.Gate))
			}
		case r.Error != "":
			select {
			case failed <- r.Error:
			default:
			}
		}
	})
	close(p.gone)
	p.mu.Lock()
	stopping := p.stopping
	p.mu.Unlock()
	if stopping || ctx.Err() != nil {
		return
	}
	// The gatekeeper is gone and its namespace with it: the workstation
	// has no interface left but lo. Nothing can be gained by keeping it.
	select {
	case failed <- "it stopped":
	default:
	}
	if p.workstation != "" {
		o.teardown(context.Background(), p)
		o.setStatus(p, sandbox.Status{State: sandbox.StateError, Error: "the gatekeeper stopped"})
	}
}

// gateStatus is the sandbox's status as its gate's telemetry tells it.
func gateStatus(t sandbox.GateTelemetry) sandbox.Status {
	if t.Open {
		return sandbox.Status{State: sandbox.StateGateOpen, ExitAddress: t.ExitAddress}
	}
	s := sandbox.Status{State: sandbox.StateGateLocked, LockReason: t.LockReason}
	if t.LockReason == sandbox.LockExitAddressMismatch {
		s.ExitAddress = t.ExitAddress
	}
	return s
}

// workstationMounts are what a workstation mounts beyond its image and
// its memory: the sandbox's Home, which Docker makes, with the sandbox's
// label, where it is not there; its /tmp; the packs' volumes, which it
// only reads; and the Shared Folder the sandbox is given, where it is
// given one.
//
// /tmp is a volume with no name, on the VM's disk: Docker makes one
// with each workstation, empty and with the owner and the mode of the
// image's /tmp, and removes it with that workstation. Programs run from
// it, as they do from the Home.
func workstationMounts(sandboxID string, packs []pack.Manifest, sharedFolder string) []mount.Mount {
	label := func() *mount.VolumeOptions {
		return &mount.VolumeOptions{Labels: map[string]string{labelSandbox: sandboxID}}
	}
	mounts := []mount.Mount{
		{Type: mount.TypeVolume, Source: HomeVolume(sandboxID), Target: sandbox.HomePath, VolumeOptions: label()},
		{Type: mount.TypeVolume, Target: sandbox.TmpPath, VolumeOptions: label()},
	}
	for _, m := range packs {
		for _, v := range m.RuntimeLayer.Volumes {
			mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: v.Name, Target: v.Path, ReadOnly: true})
		}
	}
	if sharedFolder != "" {
		// A bind, not a volume: Docker refuses it where the folder is not
		// there, which is where the VM does not mount the Mac's.
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: sharedFolder, Target: sandbox.SharedFolderPath})
	}
	return mounts
}

// openLink opens the sandbox's Link and has it served. The Link is the
// standard streams of the supervisor's link, run in the workstation as
// the agent's user: it is carried by the Docker API, as the Packet
// stream is, so nothing listens for it on the Mac and no port of the
// sandbox is published. It ends with the sandbox; if it ends before,
// the sandbox goes on, and what asks something of the app there is told
// that the Link is not open.
func (o *Orchestrator) openLink(ctx, podCtx context.Context, p *pod, serve func(io.ReadWriteCloser)) error {
	stream, err := o.openExecStream(ctx, p.workstation, sandboximage.WorkstationUser, io.Discard, sandboximage.SupervisorPath, "link")
	if err != nil {
		return fmt.Errorf("opening the sandbox's Link: %w", err)
	}
	go func() {
		<-podCtx.Done()
		stream.Close()
	}()
	go serve(endsWithItsSandbox{stream, podCtx.Done()})
	return nil
}

// endsWithItsSandbox is a stream that the end of its sandbox closes:
// whoever reads it is then told that it ended, as a stream does, and
// not that it broke.
type endsWithItsSandbox struct {
	io.ReadWriteCloser
	ended <-chan struct{}
}

func (s endsWithItsSandbox) Read(p []byte) (int, error) {
	n, err := s.ReadWriteCloser.Read(p)
	if err != nil {
		select {
		case <-s.ended:
			err = io.EOF
		default:
		}
	}
	return n, err
}

// startWorkstation creates and starts the container the agent works in.
func (o *Orchestrator) startWorkstation(ctx, podCtx context.Context, p *pod, image, timezone string, packs []pack.Manifest) error {
	cfg, sharedFolder := p.cfg, p.sharedFolder
	// That the workstation mounts a Shared Folder is the supervisor's to
	// know: where it mounts none, an empty folder one left in the Home
	// goes.
	supervisor := sandbox.SupervisorConfig{SharedFolder: sharedFolder != ""}
	if cfg.HostTools.Enabled {
		// The supervisor registers the server by which the agent runs
		// Host Tools, and tells the agent what their Wall was given.
		supervisor.HostTools = &sandbox.HostToolsNotice{GPU: cfg.HostTools.GPU, MacName: cfg.HostTools.MacName, Applications: cfg.HostTools.Applications}
	}
	env := []string{"TZ=" + timezone}
	// Where an agent that is sent to a registry of the app's own finds it.
	registries := sandbox.RegistryEnvironment()
	for _, name := range slices.Sorted(maps.Keys(registries)) {
		env = append(env, name+"="+registries[name])
	}
	mounts := workstationMounts(cfg.ID, packs, sharedFolder)
	for _, m := range packs {
		for k, v := range m.RuntimeLayer.Env {
			env = append(env, k+"="+v)
		}
		if d := m.DaemonProcess; d != nil {
			supervisor.Daemons = append(supervisor.Daemons, sandbox.Daemon{Pack: m.Name, Command: d.Command, ReadinessPort: d.ReadinessPort})
		}
		if b := m.MCPBridge; b != nil {
			supervisor.MCPServers = append(supervisor.MCPServers, sandbox.MCPServer{Name: b.Name, Command: b.Command, Env: b.Env})
		}
	}
	supervisorJSON, _ := json.Marshal(supervisor)
	env = append(env, sandbox.SupervisorConfigEnv+"="+string(supervisorJSON))

	pids := int64(workstationPids)
	created, err := o.docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: WorkstationContainer(cfg.ID),
		Config: &container.Config{
			Image:  image,
			User:   sandboximage.WorkstationUser,
			Env:    env,
			Labels: labels(cfg.ID, roleWorkstation),
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(namespaceOf(p.gatekeeper)),
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			ShmSize:        int64(cfg.Resources.ShmMB) << 20,
			Mounts:         mounts,
			RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
			LogConfig:      logConfig,
			Resources: container.Resources{
				NanoCPUs:  int64(cfg.Resources.CPUs * 1e9),
				Memory:    int64(cfg.Resources.MemoryMB) << 20,
				PidsLimit: &pids,
			},
		},
	})
	if err != nil {
		if sharedFolder != "" && strings.Contains(err.Error(), sharedFolder) {
			return fmt.Errorf("the VM does not see the sandbox's Shared Folder: start the Colima profile with --mount %s:w (%w)", filepath.Dir(sharedFolder), err)
		}
		return fmt.Errorf("creating the workstation: %w", err)
	}
	if _, err := o.docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting the workstation: %w", err)
	}
	p.workstation = created.ID

	go o.tail(podCtx, p.workstation, func(line []byte) {
		var r sandbox.SupervisorReport
		if json.Unmarshal(line, &r) != nil {
			return
		}
		switch {
		case r.Status != nil:
			o.listen(sandbox.Update{SandboxID: cfg.ID, Workstation: r.Status})
		case r.MCPCall != nil:
			o.listen(sandbox.Update{SandboxID: cfg.ID, MCPCall: r.MCPCall})
		}
	})
	return nil
}
