// Package viewport reads Viewport Streams: frames out of, and input
// into, the display of a Capability Pack's daemon. It knows nothing of
// the dashboard that shows them or of Docker, which carries its
// connections: it is given a way to connect and returns images.
package viewport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"golang.org/x/net/websocket"
)

// Dial opens a new connection to the stream's port in the sandbox.
type Dial func(ctx context.Context) (io.ReadWriteCloser, error)

// Frame is one picture of the display.
type Frame struct {
	Image image.Image
	// PageWidth and PageHeight are the display's size in the units input
	// positions are given in, which is not the image's size when the
	// stream scales the picture down.
	PageWidth, PageHeight float64
}

// BrowserTab is one of the pages a browser has open, as the browser
// lists it.
type BrowserTab struct {
	// ID is the browser's own name for it.
	ID string
	// Title is what the page calls itself, and Address where it is. Both
	// are the page's words, not the app's.
	Title, Address string
}

// Stream is a `cdp_screencast` Viewport Stream: the screencast of one
// Browser Tab of a browser that speaks the Chrome DevTools Protocol.
type Stream struct {
	// dial and host are how the browser is reached, and tab the ID of
	// the Browser Tab the stream shows.
	dial Dial
	host string
	tab  string

	ws     *websocket.Conn
	frames chan screencastFrame
	done   chan struct{}
	err    error

	mu     sync.Mutex // guards writes and nextID
	nextID int
	// unacked is the frame last returned by Next. The browser sends the
	// one after it only once this one is acknowledged.
	unacked int
}

// screencastFrame is the Page.screencastFrame event.
type screencastFrame struct {
	Data      string `json:"data"`
	SessionID int    `json:"sessionId"`
	Metadata  struct {
		DeviceWidth  float64 `json:"deviceWidth"`
		DeviceHeight float64 `json:"deviceHeight"`
	} `json:"metadata"`
}

// Size of the frames asked for. The browser scales the page down to fit.
const (
	MaxFrameWidth  = 1280
	MaxFrameHeight = 720
	jpegQuality    = 70
)

// OpenCDP starts the screencast of one of the browser's Browser Tabs:
// the one whose ID is tab, which is brought to the front, or the first
// the browser lists where tab is empty. port is the browser's debugging
// port as the browser itself knows it.
func OpenCDP(ctx context.Context, dial Dial, port int, tab string) (*Stream, error) {
	host := fmt.Sprintf("127.0.0.1:%d", port)
	tabs, err := browserTabs(ctx, dial, host)
	if err != nil {
		return nil, err
	}
	if len(tabs) == 0 {
		return nil, errors.New("the browser has no page open")
	}
	page, chosen := tabs[0].ID, tab != ""
	if chosen {
		// The ID goes into a path: it is one the browser listed, or none.
		if !slices.ContainsFunc(tabs, func(t BrowserTab) bool { return t.ID == tab }) {
			return nil, errors.New("the browser no longer has that Browser Tab")
		}
		page = tab
	}
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	// The browser refuses hosts and origins that could be a web page's:
	// both must be its own loopback address.
	config, err := websocket.NewConfig("ws://"+host+"/devtools/page/"+page, "http://"+host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	ws, err := websocket.NewClient(config, conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("connecting to the page: %w", err)
	}
	s := &Stream{dial: dial, host: host, tab: page, ws: ws, frames: make(chan screencastFrame, 4), done: make(chan struct{})}
	go s.read()
	if chosen {
		// A Browser Tab behind another draws nothing.
		if err := s.send("Page.bringToFront", map[string]any{}); err != nil {
			s.Close()
			return nil, err
		}
	}
	err = s.send("Page.startScreencast", map[string]any{
		"format":        "jpeg",
		"quality":       jpegQuality,
		"maxWidth":      MaxFrameWidth,
		"maxHeight":     MaxFrameHeight,
		"everyNthFrame": 1,
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Tab is the ID of the Browser Tab the stream shows.
func (s *Stream) Tab() string { return s.tab }

// Tabs lists the browser's Browser Tabs, in the browser's order, on a
// connection of its own.
func (s *Stream) Tabs(ctx context.Context) ([]BrowserTab, error) {
	return browserTabs(ctx, s.dial, s.host)
}

// Navigate sends the Browser Tab the stream shows to an address.
func (s *Stream) Navigate(address string) error {
	return s.send("Page.navigate", map[string]any{"url": address})
}

// Address makes an address of what was typed into an Address Bar: as it
// is where it names its scheme, and otherwise over HTTPS, or over HTTP
// where it is the sandbox's own loopback, which serves no certificate.
func Address(typed string) string {
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return ""
	}
	if scheme, _, found := strings.Cut(typed, ":"); found && (strings.Contains(typed, "://") || scheme == "about" || scheme == "data" || scheme == "view-source") {
		return typed
	}
	host := typed
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.Trim(host, "[]")
	if ip, err := netip.ParseAddr(host); host == "localhost" || err == nil && ip.IsLoopback() {
		return "http://" + typed
	}
	return "https://" + typed
}

// browserTabs lists the browser's pages, asking over HTTP as the
// protocol has it.
func browserTabs(ctx context.Context, dial Dial, host string) ([]BrowserTab, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// The answer is waited for only as long as ctx lasts.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	req, _ := http.NewRequest(http.MethodGet, "http://"+host+"/json/list", nil)
	req.Close = true
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return nil, fmt.Errorf("asking the browser for its pages: %w", err)
	}
	defer res.Body.Close()
	var targets []struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&targets); err != nil {
		return nil, fmt.Errorf("asking the browser for its pages: %w", err)
	}
	var tabs []BrowserTab
	for _, t := range targets {
		if t.Type == "page" {
			tabs = append(tabs, BrowserTab{ID: t.ID, Title: t.Title, Address: t.URL})
		}
	}
	return tabs, nil
}

// send sends a command. Its reply is not waited for: commands here either
// have none worth reading or fail by ending the stream.
func (s *Stream) send(method string, params any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return websocket.JSON.Send(s.ws, map[string]any{"id": s.nextID, "method": method, "params": params})
}

func (s *Stream) read() {
	defer close(s.done)
	for {
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := websocket.JSON.Receive(s.ws, &msg); err != nil {
			s.err = err
			return
		}
		if msg.Method != "Page.screencastFrame" {
			continue
		}
		var f screencastFrame
		if json.Unmarshal(msg.Params, &f) != nil {
			continue
		}
		select {
		case s.frames <- f:
		default:
			// The reader of frames is behind by more than the browser
			// sends unacknowledged; this frame is lost and the next
			// acknowledgement brings a newer one.
		}
	}
}

// Next returns the next frame. Calling it says that the frame before was
// shown: only then does the browser send another, so a stream nobody is
// looking at costs nothing. It blocks until a frame comes, the stream
// ends, or ctx does.
func (s *Stream) Next(ctx context.Context) (Frame, error) {
	if s.unacked != 0 {
		if err := s.send("Page.screencastFrameAck", map[string]any{"sessionId": s.unacked}); err != nil {
			return Frame{}, err
		}
		s.unacked = 0
	}
	for {
		select {
		case f := <-s.frames:
			s.unacked = f.SessionID
			data, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				continue
			}
			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				continue
			}
			frame := Frame{Image: img, PageWidth: f.Metadata.DeviceWidth, PageHeight: f.Metadata.DeviceHeight}
			if frame.PageWidth <= 0 || frame.PageHeight <= 0 {
				frame.PageWidth, frame.PageHeight = float64(img.Bounds().Dx()), float64(img.Bounds().Dy())
			}
			return frame, nil
		case <-s.done:
			return Frame{}, fmt.Errorf("the stream ended: %w", s.err)
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
}

// Close ends the stream.
func (s *Stream) Close() error { return s.ws.Close() }
