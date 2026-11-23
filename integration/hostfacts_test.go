//go:build integration

package integration

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"zeolite/internal/sandbox"
)

// A host fact is something that tells this Mac, or the network it is
// on, from any other: its name, its addresses, its resolvers, its Wi-Fi
// network. A sandbox holds none.

type hostFact struct {
	what, value string
	// guessable is true for a fact someone could write down without
	// having learned it: a private IPv4 address, of which there are few
	// enough to list or to scan, and a public resolver everyone names,
	// where this Mac's network uses one. Finding one in what a sandbox
	// holds is a finding all the same. Finding one in what a model wrote
	// is not: its scans print every address they try.
	guessable bool
}

// usualResolvers are the public resolvers a list of resolvers to try
// begins with.
var usualResolvers = map[string]bool{
	"1.1.1.1": true, "1.0.0.1": true, // Cloudflare
	"8.8.8.8": true, "8.8.4.4": true, // Google
	"9.9.9.9": true, "149.112.112.112": true, // Quad9
	"208.67.222.222": true, "208.67.220.220": true, // OpenDNS
}

// unguessable returns the facts that nobody writes down by chance: names,
// hardware addresses, IPv6 addresses, the Wi-Fi network.
func unguessable(facts []hostFact) []hostFact {
	var out []hostFact
	for _, f := range facts {
		if !f.guessable {
			out = append(out, f)
		}
	}
	return out
}

func commandOutput(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out))
}

// macAddresses are this Mac's own IPv4 addresses, loopback aside.
func macAddresses(t *testing.T) []netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var out []netip.Addr
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if addr, ok := netip.AddrFromSlice(ipnet.IP); ok && addr.Unmap().Is4() && !addr.Unmap().IsLoopback() {
			out = append(out, addr.Unmap())
		}
	}
	return out
}

var (
	nameserverLine = regexp.MustCompile(`(?m)^\s*nameserver\[\d+\]\s*:\s*(\S+)`)
	gatewayLine    = regexp.MustCompile(`(?m)^\s*gateway:\s*(\S+)`)
	ssidLine       = regexp.MustCompile(`(?m)^\s*SSID : (.+)$`)
)

// macFacts reads this Mac's host facts, as they are now.
func macFacts(t *testing.T) []hostFact {
	t.Helper()
	var facts []hostFact
	add := func(what, value string) {
		value = strings.TrimSpace(value)
		// A value this short would be found in anything.
		if len(value) < 4 {
			return
		}
		for _, f := range facts {
			if f.value == value {
				return
			}
		}
		addr, err := netip.ParseAddr(value)
		facts = append(facts, hostFact{what: what, value: value, guessable: err == nil && addr.Is4() && (addr.IsPrivate() || usualResolvers[value])})
	}
	name, _ := os.Hostname()
	for _, n := range []string{name, commandOutput("scutil", "--get", "LocalHostName"), commandOutput("scutil", "--get", "ComputerName"), commandOutput("scutil", "--get", "HostName")} {
		add("the Mac's name", n)
		add("the Mac's name", strings.TrimSuffix(n, ".local"))
	}
	for _, addr := range macAddresses(t) {
		add("one of the Mac's addresses", addr.String())
	}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if len(iface.HardwareAddr) > 0 {
			add("one of the Mac's hardware addresses", iface.HardwareAddr.String())
		}
		addrs, _ := iface.Addrs()
		if iface.Flags&net.FlagLoopback != 0 {
			// Every Mac's loopback has the same addresses, fe80::1 among
			// them: they tell this Mac from no other.
			addrs = nil
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() == nil && !ipnet.IP.IsLoopback() {
				add("one of the Mac's IPv6 addresses", ipnet.IP.String())
			}
		}
		if strings.HasPrefix(iface.Name, "en") {
			if m := ssidLine.FindStringSubmatch(commandOutput("ipconfig", "getsummary", iface.Name)); m != nil && m[1] != "<redacted>" {
				add("the Mac's Wi-Fi network", m[1])
			}
		}
	}
	for _, m := range nameserverLine.FindAllStringSubmatch(commandOutput("scutil", "--dns"), -1) {
		if addr, err := netip.ParseAddr(m[1]); err == nil && !addr.IsLoopback() {
			add("one of the Mac's DNS servers", addr.String())
		}
	}
	if m := gatewayLine.FindStringSubmatch(commandOutput("route", "-n", "get", "default")); m != nil {
		add("the Mac's default router", m[1])
	}
	return facts
}

// held returns the facts a text holds. An address counts only as a
// whole: 10.0.0.2 is not found in 110.0.0.25.
func held(text string, facts []hostFact) []hostFact {
	var found []hostFact
	lower := strings.ToLower(text)
	for _, f := range facts {
		value := strings.ToLower(f.value)
		for from := 0; ; {
			i := strings.Index(lower[from:], value)
			if i < 0 {
				break
			}
			i += from
			end := i + len(value)
			partOfMore := func(b byte) bool { return b == '.' || b == ':' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') }
			_, isAddress := netip.ParseAddr(f.value)
			if isAddress != nil || !((i > 0 && partOfMore(lower[i-1])) || (end < len(lower) && partOfMore(lower[end]))) {
				found = append(found, f)
				break
			}
			from = end
		}
	}
	return found
}

var (
	ipv4Token = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	ipv6Token = regexp.MustCompile(`(?i)(?:^|[\s\[=,])((?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4})(?:$|[\s\]/%,])`)
)

// hostsFileIPv6 are the IPv6 entries Docker writes in every hosts file.
var hostsFileIPv6 = map[string]bool{"::1": true, "fe00::": true, "ff00::": true, "ff02::1": true, "ff02::2": true}

// foreignAddresses returns the addresses in a text that are neither
// loopback nor the sandbox's own.
func foreignAddresses(text string, own netip.Prefix) []string {
	var foreign []string
	for _, token := range ipv4Token.FindAllString(text, -1) {
		addr, err := netip.ParseAddr(token)
		if err != nil {
			continue // 22.11.0.300 is a version, not an address
		}
		if !addr.IsLoopback() && !own.Contains(addr) {
			foreign = append(foreign, token)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		for _, m := range ipv6Token.FindAllStringSubmatch(line, -1) {
			addr, err := netip.ParseAddr(m[1])
			if err != nil || hostsFileIPv6[addr.String()] {
				continue
			}
			foreign = append(foreign, m[1])
		}
	}
	return foreign
}

func TestSandboxHoldsNoHostFacts(t *testing.T) {
	cfg := lockedConfig("host-facts", "Host facts")
	start(t, cfg, nil, nil)
	waitState(t, cfg.ID, sandbox.StateGateLocked, 20*time.Second)

	// The one address that is the sandbox's own.
	own := netip.PrefixFrom(sandbox.SandboxAddress, 32)

	// What the workstation is told about where it is.
	told := map[string]string{}
	for what, script := range map[string]string{
		"/etc/hosts":       "cat /etc/hosts",
		"/etc/resolv.conf": "cat /etc/resolv.conf",
		"the hostname":     "hostname; cat /etc/hostname /proc/sys/kernel/hostname /proc/sys/kernel/domainname",
		"the environment":  "env",
	} {
		out, code := inSandbox(cfg.ID, script)
		if code != 0 || trimmed(out) == "" {
			t.Fatalf("reading %s in the workstation: %q (exit %d)", what, out, code)
		}
		told[what] = out
	}
	// And what it can read of the kernel it runs on.
	kernel, _ := inSandbox(cfg.ID, "cat /proc/cmdline /proc/version /proc/mounts /proc/self/mountinfo /proc/net/route /proc/net/arp /proc/net/fib_trie /proc/net/if_inet6 2>/dev/null")

	for what, text := range told {
		for _, addr := range foreignAddresses(text, own) {
			// The sandbox's resolver is the one address it is given: a
			// public one, which its config names.
			if addr == cfg.Gate.Resolver {
				continue
			}
			t.Errorf("%s holds %s, an address that is neither loopback nor the sandbox's own %s:\n%s", what, addr, own.Addr(), text)
		}
		for _, word := range []string{"docker.internal", "lima", "colima"} {
			if strings.Contains(strings.ToLower(text), word) {
				t.Errorf("%s says %q:\n%s", what, word, text)
			}
		}
	}

	facts := macFacts(t)
	if len(facts) < 3 {
		t.Fatalf("only %d host facts could be read from this Mac: the test would prove little", len(facts))
	}
	kinds := map[string]int{}
	for _, f := range facts {
		kinds[f.what]++
	}
	t.Logf("looked for %d host facts: %v", len(facts), kinds)
	told["what the kernel shows"] = kernel
	for what, text := range told {
		for _, f := range held(text, facts) {
			t.Errorf("%s holds %s", what, f.what)
		}
	}

	// The search finds a fact where there is one.
	for _, f := range facts {
		sample := "nameserver " + f.value + "\n"
		if found := held(sample, facts); len(found) == 0 {
			t.Errorf("the search missed %s in a text that holds it", f.what)
		}
	}
	if got := foreignAddresses("nameserver 192.168.5.3\n127.0.0.1 localhost\n"+own.Addr().String()+" sandbox\n", own); len(got) != 1 || got[0] != "192.168.5.3" {
		t.Errorf("the search for foreign addresses found %v, want the one that is foreign", got)
	}
}
