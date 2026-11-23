package probe

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// echo is an HTTPS server that answers as a probe endpoint does.
type echo struct {
	server *httptest.Server
	mu     sync.Mutex
	// answers are the bodies by path, and conns counts connections.
	answers map[string]string
	conns   int
}

func newEcho(t *testing.T) *echo {
	e := &echo{answers: map[string]string{"/ip": "203.0.113.7\n", "/timezone": "Europe/Berlin\n"}}
	e.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		body, ok := e.answers[r.URL.Path]
		e.mu.Unlock()
		switch {
		case r.URL.Path == "/elsewhere":
			http.Redirect(w, r, "/ip", http.StatusFound)
		case !ok:
			http.NotFound(w, r)
		default:
			w.Write([]byte(body))
		}
	}))
	e.server.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			e.mu.Lock()
			e.conns++
			e.mu.Unlock()
		}
	}
	e.server.StartTLS()
	t.Cleanup(e.server.Close)
	return e
}

func (e *echo) answer(path, body string) {
	e.mu.Lock()
	e.answers[path] = body
	e.mu.Unlock()
}

func (e *echo) connections() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conns
}

// pin is the pin of the key the server presents.
func (e *echo) pin() string {
	sum := sha256.Sum256(e.server.Certificate().RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// dial connects to the server whatever address is asked for, as the
// internet would carry a probe to its endpoint.
func (e *echo) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp4", e.server.Listener.Addr().String())
}

// client makes a Client that reaches the echo, with its pin unless
// another is given.
func (e *echo) client(t *testing.T, pin *string) *Client {
	t.Helper()
	p := e.pin()
	if pin != nil {
		p = *pin
	}
	c, err := newClient(p, e.dial)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProbe(t *testing.T) {
	e := newEcho(t)
	ctx := context.Background()
	const url = "https://probe.example.com/ip"

	c := e.client(t, nil)
	if a, err := c.ExitAddress(ctx, url); err != nil || a != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("a probe gave %v, %v", a, err)
	}
	// Each probe is a connection of its own: the question is where a
	// connection made now leaves from.
	c.ExitAddress(ctx, url)
	if n := e.connections(); n != 2 {
		t.Errorf("two probes made %d connections", n)
	}

	// An address is an answer whatever its family, without what it is
	// mapped in.
	for body, want := range map[string]string{"198.51.100.9": "198.51.100.9", "2001:db8::7": "2001:db8::7", "::ffff:198.51.100.9\n": "198.51.100.9"} {
		e.answer("/ip", body)
		if a, err := c.ExitAddress(ctx, url); err != nil || a.String() != want {
			t.Errorf("a probe answered with %q gave %v, %v", body, a, err)
		}
	}
	// What is no address is no answer.
	for _, body := range []string{"", "not an address", "203.0.113.7 203.0.113.8", "fe80::1%en0", strings.Repeat("9", 200)} {
		e.answer("/ip", body)
		if a, err := c.ExitAddress(ctx, url); err == nil {
			t.Errorf("a probe answered with %q gave %v", body, a)
		}
	}
	e.answer("/ip", "203.0.113.7")

	// What is not an answer: another status, a redirect, another key.
	wrongPin, noPin := base64.StdEncoding.EncodeToString(make([]byte, sha256.Size)), ""
	for what, ask := range map[string]func() (netip.Addr, error){
		"a path the endpoint does not have": func() (netip.Addr, error) { return c.ExitAddress(ctx, "https://probe.example.com/missing") },
		"a redirect":                        func() (netip.Addr, error) { return c.ExitAddress(ctx, "https://probe.example.com/elsewhere") },
		"a key that does not match the pin": func() (netip.Addr, error) { return e.client(t, &wrongPin).ExitAddress(ctx, url) },
		"a certificate no authority signed": func() (netip.Addr, error) { return e.client(t, &noPin).ExitAddress(ctx, url) },
	} {
		if a, err := ask(); err == nil {
			t.Errorf("%s gave %v, want the probe unanswered", what, a)
		}
	}

	// A probe that hangs is unanswered when its time is up.
	slow, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	hung := e.client(t, nil)
	hung.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if a, err := hung.ExitAddress(slow, url); err == nil {
		t.Errorf("a probe that hung gave %v", a)
	}

	// Fetch returns what was answered, trimmed, and no more than an
	// address takes.
	if tz, err := c.Fetch(ctx, "https://probe.example.com/timezone"); err != nil || tz != "Europe/Berlin" {
		t.Errorf("fetching the timezone gave %q, %v", tz, err)
	}
	e.answer("/timezone", strings.Repeat("x", 500))
	if body, err := c.Fetch(ctx, "https://probe.example.com/timezone"); err != nil || len(body) != bodyLimit {
		t.Errorf("a long answer was read to %d bytes (%v), want %d", len(body), err, bodyLimit)
	}
}

// A Client is made with a socket option, a public resolver and a pin
// that is one, or not at all. It keeps no connection, follows no
// redirect and asks no proxy, whatever the environment names.
func TestAClientIsMadeOnlyAsAProbeAsks(t *testing.T) {
	control := func(string, string, syscall.RawConn) error { return nil }
	resolver := netip.MustParseAddr("1.1.1.1")
	if _, err := New(resolver, "", nil); err == nil {
		t.Error("a Client was made with no socket option")
	}
	for _, r := range []string{"192.168.1.1", "127.0.0.1", "2606:4700:4700::1111", "100.100.100.100"} {
		if _, err := New(netip.MustParseAddr(r), "", control); err == nil {
			t.Errorf("a Client was made with the resolver %s", r)
		}
	}
	if _, err := New(netip.Addr{}, "", control); err == nil {
		t.Error("a Client was made with no resolver")
	}
	for _, pin := range []string{"AAAA", "not base64", base64.StdEncoding.EncodeToString(make([]byte, 20))} {
		if _, err := New(resolver, pin, control); err == nil {
			t.Errorf("a Client was made with the pin %q", pin)
		}
	}

	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:9")
	c, err := New(resolver, "", control)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || !transport.DisableKeepAlives || transport.DialContext == nil || transport.DialTLSContext != nil {
		t.Fatalf("the transport is %+v", c.http.Transport)
	}
	if c.http.Timeout != HoldTimeout || c.http.CheckRedirect == nil || c.http.Jar != nil {
		t.Errorf("the client is %+v", c.http)
	}
}

// The socket option is on every socket a Client makes: the one to the
// resolver and the one to the endpoint. One that fails makes no socket.
func TestEverySocketCarriesTheSocketOption(t *testing.T) {
	var mu sync.Mutex
	var controlled []string
	refused := errors.New("refused by the test")
	control := func(network, address string, _ syscall.RawConn) error {
		mu.Lock()
		defer mu.Unlock()
		controlled = append(controlled, network+" "+address)
		return refused
	}
	c, err := New(netip.MustParseAddr("1.1.1.1"), "", control)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// An address: one socket, to it. A name: the resolver first, and
	// nothing after a lookup that failed.
	if _, err := c.ExitAddress(ctx, "https://203.0.113.9/ip"); !errors.Is(err, refused) {
		t.Errorf("a probe whose socket option failed gave %v", err)
	}
	if _, err := c.ExitAddress(ctx, "https://probe.example.com/ip"); err == nil {
		t.Error("a probe whose name could not be looked up was answered")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(controlled) < 2 || controlled[0] != "tcp4 203.0.113.9:443" {
		t.Fatalf("the socket option was asked for %q", controlled)
	}
	for _, socket := range controlled[1:] {
		if socket != "udp4 1.1.1.1:53" && socket != "tcp4 1.1.1.1:53" {
			t.Errorf("a socket was made for %q: a name is looked up at the resolver alone", socket)
		}
	}
}

// The probe's sockets go to port 443 of public IPv4 addresses and
// nowhere else, whatever a probe URL names or a resolver answers.
func TestDialer(t *testing.T) {
	var looked []string
	var connected []netip.AddrPort
	answers := map[string][]netip.Addr{
		"probe.example.com":   {netip.MustParseAddr("203.0.113.80")},
		"inside.example.com":  {netip.MustParseAddr("192.168.5.2")},
		"mixed.example.com":   {netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("::ffff:127.0.0.1"), netip.MustParseAddr("203.0.113.81")},
		"nowhere.example.com": nil,
	}
	d := &dialer{
		lookup: func(_ context.Context, name string) ([]netip.Addr, error) {
			looked = append(looked, name)
			return answers[name], nil
		},
		connect: func(_ context.Context, to netip.AddrPort) (net.Conn, error) {
			connected = append(connected, to)
			client, server := net.Pipe()
			server.Close()
			return client, nil
		},
		now: time.Now,
	}
	tests := []struct {
		address string
		want    string // where it connects, empty when it must not
	}{
		{"probe.example.com:443", "203.0.113.80:443"},
		{"203.0.113.9:443", "203.0.113.9:443"},
		{"mixed.example.com:443", "203.0.113.81:443"},
		{"probe.example.com:80", ""},
		{"probe.example.com:8443", ""},
		{"inside.example.com:443", ""},
		{"nowhere.example.com:443", ""},
		{"127.0.0.1:443", ""},
		{"192.168.5.2:443", ""},
		{"172.30.0.2:443", ""},
		{"169.254.169.254:443", ""},
		{"198.18.0.1:443", ""},
		{"[::1]:443", ""},
		{"[2606:4700:4700::1111]:443", ""},
		{"[::ffff:10.0.0.1]:443", ""},
	}
	for _, tt := range tests {
		connected = nil
		c, err := d.dial(context.Background(), "tcp", tt.address)
		got := ""
		if len(connected) > 0 {
			got = connected[len(connected)-1].String()
		}
		if got != tt.want || len(connected) > 1 || (err == nil) != (tt.want != "") {
			t.Errorf("dialing %s connected to %v (error %v), want %q", tt.address, connected, err, tt.want)
		}
		if c != nil {
			c.Close()
		}
	}
	if joined := strings.Join(looked, " "); strings.Contains(joined, "203.0.113.9") || !strings.Contains(joined, "probe.example.com") {
		t.Errorf("the dialer looked up %q: an address needs no lookup, a name does", joined)
	}
}

// A probe that is sent by an Interface where names are answered with
// stand-in addresses connects to one, by a name or by itself, and to
// nothing else that is not public. No other probe connects to one. What
// is kept of a name is kept apart for the two: a name that stood at a
// stand-in address by one Interface is looked up again for a probe that
// reaches none.
func TestDialerReachesStandInAddressesOnlyForAProbeThatSaysSo(t *testing.T) {
	lookups := 0
	var connected []netip.AddrPort
	answer := []netip.Addr{netip.MustParseAddr("198.18.0.13")}
	d := &dialer{
		lookup: func(_ context.Context, name string) ([]netip.Addr, error) {
			lookups++
			if name == "inside.example.com" {
				return []netip.Addr{netip.MustParseAddr("192.168.5.2")}, nil
			}
			return answer, nil
		},
		connect: func(_ context.Context, to netip.AddrPort) (net.Conn, error) {
			connected = append(connected, to)
			client, server := net.Pipe()
			server.Close()
			return client, nil
		},
		now: time.Now,
	}
	plain, standIns := context.Background(), ReachingStandIns(context.Background())
	if ReachesStandIns(plain) || !ReachesStandIns(standIns) {
		t.Fatal("a context does not say what it was made to say")
	}
	within, cancel := context.WithTimeout(standIns, time.Minute)
	defer cancel()
	if !ReachesStandIns(within) {
		t.Error("a context that comes of one that reaches stand-in addresses does not")
	}
	dial := func(ctx context.Context, address string) string {
		t.Helper()
		connected = nil
		c, err := d.dial(ctx, "tcp", address)
		if c != nil {
			c.Close()
		}
		if len(connected) > 1 || (err == nil) != (len(connected) == 1) {
			t.Fatalf("dialing %s connected to %v with the error %v", address, connected, err)
		}
		if len(connected) == 0 {
			return ""
		}
		return connected[0].String()
	}

	if got := dial(plain, "probe.example.com:443"); got != "" {
		t.Errorf("a probe that reaches no stand-in address connected to %s", got)
	}
	for range 2 {
		if got := dial(standIns, "probe.example.com:443"); got != "198.18.0.13:443" {
			t.Errorf("a probe that reaches stand-in addresses connected to %q, want the one its name stands at", got)
		}
	}
	// One lookup for the probe that reaches none, which keeps nothing it
	// can use, and one for the two that do.
	if lookups != 2 {
		t.Errorf("%d lookups, want two", lookups)
	}
	// By another Interface the name is elsewhere: what was kept for the
	// probes that reach stand-in addresses is not what this one is given.
	answer = []netip.Addr{netip.MustParseAddr("203.0.113.80")}
	if got := dial(plain, "probe.example.com:443"); got != "203.0.113.80:443" {
		t.Errorf("once the name was at a public address, a probe that reaches no stand-in address connected to %q", got)
	}
	if got := dial(standIns, "probe.example.com:443"); got != "198.18.0.13:443" {
		t.Errorf("a probe that reaches stand-in addresses connected to %q, want where its name was kept", got)
	}

	for address, want := range map[string]string{
		"198.19.255.254:443":     "198.19.255.254:443",
		"203.0.113.9:443":        "203.0.113.9:443",
		"198.18.0.13:80":         "",
		"inside.example.com:443": "",
		"192.168.5.2:443":        "",
		"127.0.0.1:443":          "",
		"10.0.0.1:443":           "",
		"100.64.0.1:443":         "",
		"169.254.169.254:443":    "",
		"[::1]:443":              "",
	} {
		if got := dial(standIns, address); got != want {
			t.Errorf("a probe that reaches stand-in addresses, dialing %s, connected to %q, want %q", address, got, want)
		}
	}
	if got := dial(plain, "198.18.0.13:443"); got != "" {
		t.Errorf("a probe that reaches no stand-in address connected to %s", got)
	}
}

// What a probe says of itself reaches the dial of its connection,
// through the client: a request's context is the one its connection is
// made under.
func TestWhatAProbeSaysReachesItsDial(t *testing.T) {
	e := newEcho(t)
	var mu sync.Mutex
	var said []bool
	c, err := newClient(e.pin(), func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		said = append(said, ReachesStandIns(ctx))
		mu.Unlock()
		return e.dial(ctx, network, address)
	})
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://probe.example.com/ip"
	if _, err := c.ExitAddress(context.Background(), url); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExitAddress(ReachingStandIns(context.Background()), url); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(ReachingStandIns(context.Background()), "https://probe.example.com/timezone"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(said) != 3 || said[0] || !said[1] || !said[2] {
		t.Errorf("the dials of three probes, of which the last two reach stand-in addresses, were told %v", said)
	}
}

// The addresses of a probe URL's name are kept: a probe waits for a
// lookup the first time only, and they are looked up again each
// lookupEvery, beside the probes.
func TestDialerKeepsWhereANameIs(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	lookups := 0
	found := netip.MustParseAddr("203.0.113.80")
	var fail error
	var connected netip.AddrPort
	d := &dialer{
		lookup: func(context.Context, string) ([]netip.Addr, error) {
			mu.Lock()
			defer mu.Unlock()
			lookups++
			if fail != nil {
				return nil, fail
			}
			return []netip.Addr{found}, nil
		},
		connect: func(_ context.Context, to netip.AddrPort) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			connected = to
			client, server := net.Pipe()
			server.Close()
			return client, nil
		},
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
	}
	// dialed is where a probe of the name connects, and how many lookups
	// there have been by then.
	dialed := func(name string) (string, int) {
		t.Helper()
		c, err := d.dial(context.Background(), "tcp", name+":443")
		if err != nil {
			t.Fatalf("dialing %s: %v", name, err)
		}
		c.Close()
		mu.Lock()
		defer mu.Unlock()
		return connected.Addr().String(), lookups
	}
	// eventually waits for what a lookup beside the probes brings about.
	eventually := func(what string, holds func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !holds(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("it never came about that %s", what)
			}
		}
	}
	set := func(change func()) {
		mu.Lock()
		defer mu.Unlock()
		change()
	}

	for range 3 {
		if to, n := dialed("probe.example.com"); to != "203.0.113.80" || n != 1 {
			t.Fatalf("a probe connected to %s after %d lookups, want one lookup for all of them", to, n)
		}
	}

	// The name moves. Once lookupEvery is over, a probe still goes where
	// the name was, without waiting, and a lookup begins beside it.
	set(func() { found = netip.MustParseAddr("203.0.113.81"); now = now.Add(lookupEvery) })
	if to, _ := dialed("probe.example.com"); to != "203.0.113.80" {
		t.Errorf("the probe that began the lookup connected to %s: it waited for it", to)
	}
	eventually("a probe goes where the name is now", func() bool {
		to, _ := dialed("probe.example.com")
		return to == "203.0.113.81"
	})
	if _, n := dialed("probe.example.com"); n != 2 {
		t.Errorf("%d lookups, want the first and one more", n)
	}

	// A lookup that fails leaves what is kept.
	set(func() { fail = errors.New("no answer"); now = now.Add(lookupEvery) })
	dialed("probe.example.com")
	eventually("the lookup was made", func() bool {
		_, n := dialed("probe.example.com")
		return n == 3
	})
	if to, _ := dialed("probe.example.com"); to != "203.0.113.81" {
		t.Errorf("after a lookup that failed a probe connected to %s", to)
	}

	// A name that was never found is looked up by each probe until it is.
	if _, err := d.dial(context.Background(), "tcp", "other.example.com:443"); err == nil {
		t.Error("a name whose lookup failed was dialed")
	}
	set(func() { fail = nil })
	if to, n := dialed("other.example.com"); to != "203.0.113.81" || n != 5 {
		t.Errorf("a name found at the second try: connected to %s after %d lookups in all", to, n)
	}
}

// reaches reports how a file makes a socket or a request outside the
// one way a probe does: a net.Dialer without a Control, a package-level
// dial or lookup, a default of net or net/http, the environment's proxy,
// or anything that listens.
func reaches(file *ast.File) (found []string, dialers int) {
	forbidden := map[string]map[string]bool{
		"net": {
			"Dial": true, "DialTimeout": true, "DialTCP": true, "DialUDP": true, "DialIP": true, "DialUnix": true,
			"LookupHost": true, "LookupIP": true, "LookupAddr": true, "LookupCNAME": true, "LookupTXT": true, "LookupSRV": true, "LookupMX": true, "LookupNS": true, "LookupPort": true,
			"DefaultResolver": true, "ListenConfig": true,
		},
		"http": {
			"DefaultClient": true, "DefaultTransport": true, "Get": true, "Head": true, "Post": true, "PostForm": true,
			"ProxyFromEnvironment": true, "ProxyURL": true, "Server": true,
		},
	}
	controlled := map[*ast.SelectorExpr]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			sel, ok := node.Type.(*ast.SelectorExpr)
			if !ok {
				break
			}
			if pkg, _ := sel.X.(*ast.Ident); pkg == nil || pkg.Name != "net" || sel.Sel.Name != "Dialer" {
				break
			}
			for _, elt := range node.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, _ := kv.Key.(*ast.Ident); key != nil && key.Name == "Control" {
						controlled[sel] = true
					}
				}
			}
		case *ast.KeyValueExpr:
			if key, _ := node.Key.(*ast.Ident); key != nil && key.Name == "Proxy" {
				found = append(found, "a Proxy")
			}
		}
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		for _, prefix := range []string{"Listen", "Accept", "Serve", "FileListener", "FilePacketConn"} {
			if strings.HasPrefix(sel.Sel.Name, prefix) {
				found = append(found, sel.Sel.Name)
			}
		}
		pkg, _ := sel.X.(*ast.Ident)
		if pkg == nil {
			return true
		}
		if forbidden[pkg.Name][sel.Sel.Name] {
			found = append(found, pkg.Name+"."+sel.Sel.Name)
		}
		if pkg.Name == "net" && sel.Sel.Name == "Dialer" {
			dialers++
			if !controlled[sel] {
				found = append(found, "a net.Dialer without a Control")
			}
		}
		return true
	})
	return found, dialers
}

// A probe's socket is made one way: by a net.Dialer that carries the
// asker's socket option. The package has no other dial, looks up no
// name but at its own resolver, uses no default of net or net/http and
// no proxy, and listens on nothing.
func TestEverySocketIsMadeWithTheSocketOption(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files, dialers := 0, 0
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
			case "os/exec", "net/rpc", "net/smtp", "net/http/httputil", "golang.org/x/net/proxy":
				t.Errorf("%s imports %s", name, path)
			}
		}
		found, n := reaches(file)
		dialers += n
		for _, how := range found {
			t.Errorf("%s uses %s: a probe's socket is made by a net.Dialer with the asker's Control, and by nothing else", name, how)
		}
	}
	if files != 1 || dialers != 2 {
		t.Errorf("read %d files with %d dialers: want the one file, with the dialer to the resolver and the one to the endpoint", files, dialers)
	}

	// The guard must be able to fail.
	sample := `package x
import ("net"; "net/http")
func f() {
	var d net.Dialer
	_ = net.Dialer{Timeout: 1}
	net.Dial("tcp", "x")
	net.LookupHost("x")
	_ = net.DefaultResolver
	http.Get("x")
	_ = &http.Transport{Proxy: http.ProxyFromEnvironment}
	ln, _ := net.Listen("tcp4", ":0"); ln.Accept()
	_ = d
}`
	file, err := parser.ParseFile(token.NewFileSet(), "x.go", sample, 0)
	if err != nil {
		t.Fatal(err)
	}
	found, _ := reaches(file)
	got := strings.Join(found, ", ")
	for _, want := range []string{"a net.Dialer without a Control", "net.Dial", "net.LookupHost", "net.DefaultResolver", "http.Get", "a Proxy", "http.ProxyFromEnvironment", "Listen", "Accept"} {
		if !strings.Contains(got, want) {
			t.Errorf("the guard missed %s in code that has it: it found %s", want, got)
		}
	}
	if n := strings.Count(got, "a net.Dialer without a Control"); n != 2 {
		t.Errorf("the guard found %d dialers without a Control in code with two", n)
	}
}
