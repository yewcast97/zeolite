//go:build integration

package integration

import (
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"zeolite/internal/orchestrator"
	"zeolite/internal/sandbox"
)

// The TUN capture watches a sandbox's TUN device, from a container of
// the tests' that joins the sandbox's namespace, and says which packets
// the sandbox actually put on its way out: what reaches the device is
// what its ruleset let through, and all of what its Carrier is given.
// A gate test is not judged by what the sandbox was told ("refused") but
// by what the capture saw.

const captureImage = "zeolite-test-capture"

var buildCaptureImage = sync.OnceValue(func() string {
	cmd := exec.Command("docker", "-H", orchestrator.ColimaEndpoint(), "build", "-q", "-t", captureImage, "-")
	cmd.Stdin = strings.NewReader("FROM debian:bookworm-slim\nRUN apt-get update && apt-get install -y --no-install-recommends tcpdump && rm -rf /var/lib/apt/lists/*\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out)
	}
	return ""
})

type tunCapture struct {
	container string
}

// packet is one packet the sandbox put on its TUN device.
type packet struct {
	src, dst netip.Addr
	// srcPort and dstPort are 0 for a packet without ports, as ICMP is.
	srcPort, dstPort uint16
	// line is the packet as tcpdump printed it.
	line string
}

// captureTUN starts capturing every IP packet a running sandbox puts on
// its TUN device: what leaves the namespace, in either version of IP.
// What comes back to the sandbox says nothing of what it sent, and is
// left out.
func captureTUN(t *testing.T, sandboxID string) *tunCapture {
	t.Helper()
	if problem := buildCaptureImage(); problem != "" {
		t.Fatalf("building the capture's image: %s", problem)
	}
	c := &tunCapture{container: "zeolite-capture-" + sandboxID}
	docker("rm", "-f", c.container)
	out, code := docker("run", "-d", "--name", c.container, "--network", "container:"+orchestrator.GatekeeperContainer(sandboxID),
		// tcpdump opens the device, then gives up root for its own user.
		"--cap-drop", "ALL", "--cap-add", "NET_RAW", "--cap-add", "NET_ADMIN", "--cap-add", "SETUID", "--cap-add", "SETGID",
		captureImage, "tcpdump", "-ni", sandbox.TUNDevice, "-l", "-q", "-Q", "out", "ip or ip6")
	if code != 0 {
		t.Fatalf("starting the capture: %s", out)
	}
	t.Cleanup(func() { docker("rm", "-f", c.container) })
	time.Sleep(time.Second) // tcpdump takes a moment to attach
	return c
}

var packetLine = regexp.MustCompile(`(?m)^.*\bIP6? (\S+) > (\S+): .*$`)

// endpoint reads an address, and a port where there is one, as tcpdump
// writes them: "192.0.2.1.443", "192.0.2.1", "2001:db8::1.443".
func endpoint(s string) (netip.Addr, uint16) {
	if addr, err := netip.ParseAddr(s); err == nil {
		return addr, 0
	}
	if i := strings.LastIndexByte(s, '.'); i > 0 {
		if addr, err := netip.ParseAddr(s[:i]); err == nil {
			var port uint16
			for _, r := range s[i+1:] {
				if r < '0' || r > '9' {
					return addr, 0
				}
				port = port*10 + uint16(r-'0')
			}
			return addr, port
		}
	}
	return netip.Addr{}, 0
}

// packets returns every packet captured so far.
func (c *tunCapture) packets(t *testing.T) []packet {
	t.Helper()
	time.Sleep(500 * time.Millisecond) // let the last packets be printed
	out, _ := docker("logs", c.container)
	var packets []packet
	for _, m := range packetLine.FindAllStringSubmatch(out, -1) {
		p := packet{line: strings.TrimSpace(m[0])}
		p.src, p.srcPort = endpoint(m[1])
		p.dst, p.dstPort = endpoint(m[2])
		packets = append(packets, p)
	}
	return packets
}

// How a packet of the sandbox's travels, as tcpdump prints it.
func (p packet) tcp() bool { return strings.Contains(p.line, ": tcp ") }
func (p packet) udp() bool { return strings.Contains(p.line, ": UDP,") }

// toResolver reports whether a packet is a name query: to the resolver's
// port 53, over UDP or TCP.
func (p packet) toResolver(resolver netip.Addr) bool {
	return p.dst == resolver && p.dstPort == 53 && (p.udp() || p.tcp())
}

// expectNothing fails the test if the sandbox put any packet on its
// device.
func (c *tunCapture) expectNothing(t *testing.T) {
	t.Helper()
	for _, p := range c.packets(t) {
		t.Errorf("the sandbox put a packet on its device: %s", p.line)
		return
	}
}

// expect fails the test for every packet of the sandbox's that allowed
// turns down, once for each pair of addresses, and if the capture saw
// nothing at all. A packet that is not from the sandbox's own address,
// or is to an address that is not public, is never allowed.
func (c *tunCapture) expect(t *testing.T, what string, allowed func(packet) bool) {
	t.Helper()
	packets := c.packets(t)
	seen := map[string]bool{}
	for _, p := range packets {
		ok := p.src == sandbox.SandboxAddress && sandbox.PublicAddress(p.dst) && p.dst.Is4() && allowed(p)
		if key := p.src.String() + " > " + p.dst.String(); !ok && !seen[key] {
			seen[key] = true
			t.Errorf("the sandbox sent a packet that is not %s: %s", what, p.line)
		}
	}
	if len(packets) == 0 {
		t.Error("the capture saw no packets at all: it is not watching")
	}
}

// expectOnly fails the test if the sandbox sent anything but TCP packets
// to the allowed destinations, each an address and a port.
func (c *tunCapture) expectOnly(t *testing.T, allowed ...netip.AddrPort) {
	t.Helper()
	c.expect(t, "TCP to "+fmt.Sprint(allowed), func(p packet) bool {
		for _, a := range allowed {
			if p.tcp() && p.dst == a.Addr() && p.dstPort == a.Port() {
				return true
			}
		}
		return false
	})
}

// expectGated fails the test if the sandbox sent anything but what an
// open gate passes: TCP to public addresses, and name queries to its
// resolver.
func (c *tunCapture) expectGated(t *testing.T) {
	t.Helper()
	c.expect(t, "TCP to a public address or a name query to the resolver", func(p packet) bool {
		return p.tcp() || p.toResolver(testResolver)
	})
}

// probes reports whether a packet is the probe's: from one of the Probe
// Source Ports, which the ruleset rejects for anything of the sandbox's
// own, and a connection to port 443 or a name query. A packet to port
// 443 from any other port is the sandbox's, whatever it is for.
func (p packet) probes(resolver netip.Addr) bool {
	return sandbox.ProbeSourcePort(p.srcPort) && ((p.tcp() && p.dstPort == 443) || p.toResolver(resolver))
}

// expectProbesOnly fails the test if the sandbox sent anything but what
// leaves while its gate is locked: the probe's connections to port 443,
// and the probe's name queries, each from a Probe Source Port.
func (c *tunCapture) expectProbesOnly(t *testing.T) {
	t.Helper()
	c.expect(t, "a probe's connection to port 443 or its name query, from a Probe Source Port", func(p packet) bool {
		return p.probes(testResolver)
	})
}

// sentTo counts the packets the sandbox sent to an address.
func (c *tunCapture) sentTo(t *testing.T, addr netip.Addr) int {
	t.Helper()
	n := 0
	for _, p := range c.packets(t) {
		if p.dst == addr {
			n++
		}
	}
	return n
}

func TestEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.5.2.50391":   "192.168.5.2 50391",
		"1.1.1.1":             "1.1.1.1 0",
		"2606:4700::1111.443": "2606:4700::1111 443",
		"ff02::2":             "ff02::2 0",
	} {
		addr, port := endpoint(in)
		if got := addr.String() + " " + itoa(int(port)); got != want {
			t.Errorf("endpoint(%q) = %s, want %s", in, got, want)
		}
	}
	m := packetLine.FindStringSubmatch("01:02:03.000000 IP 192.0.0.2.40000 > 192.168.5.2.50391: tcp 0")
	if m == nil || m[1] != "192.0.0.2.40000" || m[2] != "192.168.5.2.50391" {
		t.Errorf("a tcpdump line was read as %q", m)
	}
	query := packet{line: "01:02:03.000000 IP 192.0.0.2.40000 > 1.1.1.1.53: UDP, length 29", dst: testResolver, dstPort: 53}
	if !query.udp() || query.tcp() || !query.toResolver(testResolver) || query.toResolver(netip.MustParseAddr("8.8.8.8")) {
		t.Errorf("a name query was read as TCP %v, UDP %v", query.tcp(), query.udp())
	}
}

// What is the probe's is known by its source port: the same connection
// to port 443, or the same name query, from a port the namespace hands
// out by itself is the sandbox's own.
func TestAPacketIsTheProbesByItsSourcePort(t *testing.T) {
	read := func(line string) packet {
		m := packetLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%q is no packet's line", line)
		}
		p := packet{line: line}
		p.src, p.srcPort = endpoint(m[1])
		p.dst, p.dstPort = endpoint(m[2])
		return p
	}
	for line, want := range map[string]bool{
		"01:02:03.000000 IP 192.0.0.2.61000 > 9.9.9.9.443: tcp 0":                           true,
		"01:02:03.000000 IP 192.0.0.2.61999 > 9.9.9.9.443: tcp 0":                           true,
		"01:02:03.000000 IP 192.0.0.2.61500 > 1.1.1.1.53: UDP, length 29":                   true,
		"01:02:03.000000 IP 192.0.0.2.61500 > 1.1.1.1.53: tcp 0":                            true,
		"01:02:03.000000 IP 192.0.0.2.40000 > 9.9.9.9.443: tcp 0":                           false,
		"01:02:03.000000 IP 192.0.0.2.60999 > 9.9.9.9.443: tcp 0":                           false,
		"01:02:03.000000 IP 192.0.0.2.62000 > 9.9.9.9.443: tcp 0":                           false,
		"01:02:03.000000 IP 192.0.0.2.40000 > 1.1.1.1.53: UDP, length 29":                   false,
		"01:02:03.000000 IP 192.0.0.2.61000 > 9.9.9.9.80: tcp 0":                            false,
		"01:02:03.000000 IP 192.0.0.2.61000 > 8.8.8.8.53: UDP, length 29":                   false,
		"01:02:03.000000 IP 192.0.0.2.61000 > 9.9.9.9.443: UDP, length 1200":                false,
		"01:02:03.000000 IP 192.0.0.2 > 9.9.9.9: ICMP echo request, id 1, seq 1, length 64": false,
	} {
		if got := read(line).probes(testResolver); got != want {
			t.Errorf("%q is the probe's: %v, want %v", line, got, want)
		}
	}
}
