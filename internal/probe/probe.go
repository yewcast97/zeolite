// Package probe makes a probe's request: it asks a public endpoint what
// address it was called from. Whoever asks gives the socket option its
// sockets carry: the gatekeeper the mark its ruleset knows, the Host
// Watchdog the interface of this Mac they are bound to. Both ask the
// same way, so that what one reads can be held against what the other
// reads.
package probe

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"

	"zeolite/internal/sandbox"
)

const (
	// Timeout bounds one probe, by its asker's context. An answer older
	// than a lease lasts says nothing of now, so nothing waits longer for
	// one; but a gate that keeps a Hold waits HoldTimeout, since a late
	// answer still says whether the connections it keeps may be kept.
	// No request outlasts HoldTimeout, whatever its context says.
	Timeout     = sandbox.LeaseDuration
	HoldTimeout = 10 * time.Second
	// bodyLimit is more than any address takes.
	bodyLimit = 64
	// lookupEvery is how often the addresses of a probe URL's name are
	// looked up again, and lookupTimeout bounds one such lookup: long
	// enough for the resolver to be asked a second time.
	lookupEvery   = time.Minute
	lookupTimeout = 15 * time.Second
)

// Control is the socket option a probe's sockets carry, as a
// net.Dialer's Control.
type Control = func(network, address string, c syscall.RawConn) error

// dialer opens the probe's connections: TCP over IPv4 to port 443 of a
// public address, on sockets that carry the asker's socket option. A
// name is resolved at the sandbox's resolver, on a socket with the same
// option, and never at this Mac's own: under a tunnel that one may
// answer with addresses that stand in for the real ones.
//
// The sandbox's resolver is answered for so too, by an Interface where
// names are answered with stand-in addresses. A probe that is sent by
// such an Interface says so, with ReachingStandIns, and its connection
// is then made to a stand-in address as to a public one: it is the one
// address the probe URL's name has there. No other probe's is.
//
// The addresses of a name are kept: a probe waits for a lookup only the
// first time, and a name query that is lost costs no probe. They are
// looked up again every lookupEvery, beside the probes. What is kept for
// the probes that reach stand-in addresses is kept apart: where a name
// stands by one Interface says nothing of where it is by another.
type dialer struct {
	// lookup returns the IPv4 addresses of a name.
	lookup func(ctx context.Context, name string) ([]netip.Addr, error)
	// connect opens a TCP connection.
	connect func(ctx context.Context, to netip.AddrPort) (net.Conn, error)
	now     func() time.Time

	mu    sync.Mutex
	names map[asked]*knownName
}

// asked is a name as a probe asks for it: by itself, or by an Interface
// where names are answered with stand-in addresses.
type asked struct {
	name     string
	standIns bool
}

// reachingStandIns is what the context of a probe carries that reaches
// stand-in addresses.
type reachingStandIns struct{}

// ReachingStandIns returns the context of a probe that is sent by an
// Interface where names are answered with stand-in addresses: its
// connection is made to such an address too.
func ReachingStandIns(ctx context.Context) context.Context {
	return context.WithValue(ctx, reachingStandIns{}, true)
}

// ReachesStandIns reports whether ctx is a context that
// ReachingStandIns returned, or comes of one.
func ReachesStandIns(ctx context.Context) bool {
	reaches, _ := ctx.Value(reachingStandIns{}).(bool)
	return reaches
}

// knownName is where a name was last found.
type knownName struct {
	addrs []netip.Addr
	// looked is when they were looked up, or when a lookup to replace
	// them last began.
	looked time.Time
}

func newDialer(resolver netip.Addr, control Control) *dialer {
	names := &net.Resolver{
		PreferGo: true,
		// Whatever server the resolver file names, the query goes to the
		// sandbox's resolver.
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			transport := "udp4"
			if strings.HasPrefix(network, "tcp") {
				transport = "tcp4"
			}
			d := net.Dialer{Control: control}
			return d.DialContext(ctx, transport, netip.AddrPortFrom(resolver, 53).String())
		},
	}
	return &dialer{
		lookup: func(ctx context.Context, name string) ([]netip.Addr, error) {
			// Rooted, so that no search domain is tried.
			return names.LookupNetIP(ctx, "ip4", strings.TrimSuffix(name, ".")+".")
		},
		connect: func(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
			d := net.Dialer{Control: control}
			return d.DialContext(ctx, "tcp4", to.String())
		},
		now: time.Now,
	}
}

// addresses returns where a name is: where it was last found, if it was,
// and otherwise where a lookup finds it now.
func (d *dialer) addresses(ctx context.Context, name asked) ([]netip.Addr, error) {
	d.mu.Lock()
	if known := d.names[name]; known != nil {
		if d.now().Sub(known.looked) >= lookupEvery {
			// One lookup at a time, and the probe does not wait for it.
			known.looked = d.now()
			go d.lookAgain(name)
		}
		addrs := known.addrs
		d.mu.Unlock()
		return addrs, nil
	}
	d.mu.Unlock()
	addrs, err := d.lookup(ctx, name.name)
	if err == nil {
		d.keep(name, addrs)
	}
	return addrs, err
}

// lookAgain replaces what is kept of a name with what a lookup finds. If
// the lookup fails, what is kept stays.
func (d *dialer) lookAgain(name asked) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	if addrs, err := d.lookup(ctx, name.name); err == nil {
		d.keep(name, addrs)
	}
}

// reached is whether a probe's connection is made to an address: a
// public IPv4 address, or a stand-in address for a probe that reaches
// those.
func reached(a netip.Addr, standIns bool) bool {
	a = a.Unmap()
	return a.Is4() && (sandbox.PublicAddress(a) || standIns && sandbox.StandInAddress(a))
}

// keep keeps the addresses a name was found at, of those a probe that
// asks so connects to. A name found at none is not kept: the next probe
// looks it up.
func (d *dialer) keep(name asked, addrs []netip.Addr) {
	var kept []netip.Addr
	for _, a := range addrs {
		if reached(a, name.standIns) {
			kept = append(kept, a)
		}
	}
	if addrs = kept; len(addrs) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.names == nil {
		d.names = map[asked]*knownName{}
	}
	d.names[name] = &knownName{addrs: addrs, looked: d.now()}
}

// dial is an http.Transport's DialContext.
func (d *dialer) dial(ctx context.Context, _, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if port != sandbox.ProbePort {
		return nil, fmt.Errorf("the probe reaches port %s only, not %s", sandbox.ProbePort, port)
	}
	standIns := ReachesStandIns(ctx)
	var addrs []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{literal}
	} else if addrs, err = d.addresses(ctx, asked{host, standIns}); err != nil {
		return nil, err
	}
	err = fmt.Errorf("%s has no address", host)
	for _, a := range addrs {
		a = a.Unmap()
		if !reached(a, standIns) {
			err = fmt.Errorf("%s is at %s, which is not a public IPv4 address", host, a)
			if standIns {
				err = fmt.Errorf("%s is at %s, which is neither a public IPv4 address nor a stand-in address", host, a)
			}
			continue
		}
		var c net.Conn
		if c, err = d.connect(ctx, netip.AddrPortFrom(a, 443)); err == nil {
			return c, nil
		}
	}
	return nil, err
}

// Client sends probes: each an HTTPS GET on a connection of its own.
type Client struct {
	http *http.Client
}

// New makes a Client whose sockets carry control and whose names are
// looked up at resolver. pin, when set, is the base64 SHA-256 of the
// public key the endpoints must present. A Client with no socket option
// is not made: a probe's socket is never one the system routes as it
// likes.
func New(resolver netip.Addr, pin string, control Control) (*Client, error) {
	if control == nil {
		return nil, errors.New("a probe's sockets carry a socket option, and none was given")
	}
	if !resolver.Is4() || !sandbox.PublicAddress(resolver) {
		return nil, fmt.Errorf("the resolver %s is not a public IPv4 address", resolver)
	}
	return newClient(pin, newDialer(resolver, control).dial)
}

// newClient makes a Client whose connections dial opens.
func newClient(pin string, dial func(ctx context.Context, network, address string) (net.Conn, error)) (*Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if pin != "" {
		want, err := base64.StdEncoding.DecodeString(pin)
		if err != nil || len(want) != sha256.Size {
			return nil, errors.New("the probe pin is not a base64 SHA-256")
		}
		// The pin replaces the certificate authorities: the endpoint is
		// trusted for its key and for nothing else.
		tlsConfig.InsecureSkipVerify = true
		tlsConfig.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("the probe endpoint presented no certificate")
			}
			cert, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(sum[:], want) != 1 {
				return errors.New("the probe endpoint's key does not match the pin")
			}
			return nil
		}
	}
	return &Client{http: &http.Client{
		Timeout: HoldTimeout,
		// The transport names no proxy: a probe goes where its socket
		// leads, whatever this process's environment says.
		Transport: &http.Transport{
			DialContext:     dial,
			TLSClientConfig: tlsConfig,
			// A new connection each time: the question is where a
			// connection made now exits, not one made a minute ago.
			DisableKeepAlives: true,
		},
		// A redirect would let the endpoint send the probe elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Fetch returns the body of an HTTPS GET, trimmed.
func (c *Client) Fetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the endpoint answered %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, bodyLimit))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// ExitAddress asks one probe URL what address it was called from. What
// is no address is no answer.
func (c *Client) ExitAddress(ctx context.Context, url string) (netip.Addr, error) {
	body, err := c.Fetch(ctx, url)
	if err != nil {
		return netip.Addr{}, err
	}
	return sandbox.ParseExitAddress(body)
}
