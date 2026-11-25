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
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

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
	// asked are the commands whose answers are waited for, by their IDs.
	asking sync.Mutex
	asked  map[int]chan answer
	// unacked is the frame last returned by Next. The browser sends the
	// one after it only once this one is acknowledged.
	unacked int
	// behind is whether the browser last said that the Browser Tab is not
	// to be seen: another stands in front of it in its window.
	behind atomic.Bool
}

// answer is what the browser says to a command: its result, or why it
// was not done.
type answer struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
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
// the one whose ID is tab, or the first the browser lists where tab is
// empty. It brings no Browser Tab to the front: one that stands behind
// another in its window draws nothing, which Behind says, until Front
// is asked for. port is the browser's debugging port as the browser
// itself knows it.
func OpenCDP(ctx context.Context, dial Dial, port int, tab string) (*Stream, error) {
	host := browserHost(port)
	tabs, err := browserTabs(ctx, dial, host)
	if err != nil {
		return nil, err
	}
	if len(tabs) == 0 {
		return nil, errors.New("the browser has no page open")
	}
	page := tabs[0].ID
	if tab != "" {
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
	s := &Stream{dial: dial, host: host, tab: page, ws: ws, frames: make(chan screencastFrame, 4), done: make(chan struct{}), asked: map[int]chan answer{}}
	go s.read()
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

// Behind is whether the Browser Tab the stream shows stands behind
// another in its window, as the browser last said: it draws nothing
// there, and the stream gives no frame until it is in front again.
func (s *Stream) Behind() bool { return s.behind.Load() }

// Front brings the Browser Tab the stream shows to the front of its
// window, which is from then on the window a Browser Tab that is opened
// with no window of its own comes to the front of.
func (s *Stream) Front() error { return s.send("Page.bringToFront", map[string]any{}) }

// Tabs lists a browser's Browser Tabs, in the browser's order: the one
// last used first. port is the browser's debugging port as the browser
// itself knows it.
func Tabs(ctx context.Context, dial Dial, port int) ([]BrowserTab, error) {
	return browserTabs(ctx, dial, browserHost(port))
}

// browserHost is the host a browser is asked under: its own loopback
// address, the only one it answers to, with its debugging port.
func browserHost(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// Navigate sends the Browser Tab the stream shows to an address.
func (s *Stream) Navigate(address string) error {
	return s.send("Page.navigate", map[string]any{"url": address})
}

// CloseTab closes the Browser Tab the stream shows, which ends the
// stream. Its ID is one the browser listed when the stream was opened.
func (s *Stream) CloseTab(ctx context.Context) error {
	if err := askBrowser(ctx, s.dial, s.host, http.MethodGet, "/json/close/"+s.tab, nil); err != nil {
		return fmt.Errorf("closing the Browser Tab: %w", err)
	}
	return nil
}

// Back sends the Browser Tab the stream shows to the address before the
// one it is at in its own history, and Forward to the one after. Where
// its history has none, nothing is sent. The browser is asked for the
// history as long as ctx lasts.
func (s *Stream) Back(ctx context.Context) error { return s.along(ctx, -1) }

// Forward is Back the other way.
func (s *Stream) Forward(ctx context.Context) error { return s.along(ctx, 1) }

// along sends the Browser Tab the stream shows by entries along its
// history.
func (s *Stream) along(ctx context.Context, by int) error {
	var history struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := s.ask(ctx, "Page.getNavigationHistory", map[string]any{}, &history); err != nil {
		return err
	}
	to := history.CurrentIndex + by
	if to < 0 || to >= len(history.Entries) {
		return nil
	}
	return s.send("Page.navigateToHistoryEntry", map[string]any{"entryId": history.Entries[to].ID})
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

// The size of the window a new Browser Tab is given: the largest frame
// a stream asks for, so that its frames are the size of its page.
const (
	tabWindowWidth  = MaxFrameWidth
	tabWindowHeight = MaxFrameHeight
)

// NewTab opens a new Browser Tab at an address in the browser, and
// returns its ID. The tab has a window of its own, in the background: a
// browser draws the front tab of each of its windows and no other, so
// that each tab opened here draws whatever the others do, and it takes
// the front of no window from the tab that has it. port is the browser's
// debugging port as the browser itself knows it.
func NewTab(ctx context.Context, dial Dial, port int, address string) (string, error) {
	var opened struct {
		TargetID string `json:"targetId"`
	}
	err := commandBrowser(ctx, dial, browserHost(port), "Target.createTarget", map[string]any{
		"url":        address,
		"newWindow":  true,
		"background": true,
		"width":      tabWindowWidth,
		"height":     tabWindowHeight,
	}, &opened)
	if err != nil {
		return "", fmt.Errorf("opening a Browser Tab: %w", err)
	}
	if opened.TargetID == "" {
		return "", errors.New("opening a Browser Tab: the browser opened none")
	}
	return opened.TargetID, nil
}

// browserSocket is the path under which a browser's own websocket is
// reached, which is no page's: what the browser says of itself names it.
const browserSocket = "/devtools/browser/"

// commandBrowser has the browser itself do one command, which is no
// page's to do, on a connection of its own, and reads the browser's
// answer into result. It waits for the answer as long as ctx lasts.
func commandBrowser(ctx context.Context, dial Dial, host, method string, params, result any) error {
	var version struct {
		Socket string `json:"webSocketDebuggerUrl"`
	}
	if err := askBrowser(ctx, dial, host, http.MethodGet, "/json/version", &version); err != nil {
		return err
	}
	// The browser's word goes into a path: it is its own websocket's, or
	// nothing is asked there.
	at, err := url.Parse(version.Socket)
	if err != nil || !strings.HasPrefix(at.Path, browserSocket) || strings.Contains(at.Path, "..") || at.RawQuery != "" {
		return errors.New("the browser did not say where it takes commands")
	}
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	config, err := websocket.NewConfig("ws://"+host+at.Path, "http://"+host)
	if err != nil {
		return err
	}
	ws, err := websocket.NewClient(config, conn)
	if err != nil {
		return fmt.Errorf("connecting to the browser: %w", err)
	}
	const id = 1
	if err := websocket.JSON.Send(ws, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		var msg struct {
			ID int `json:"id"`
			answer
		}
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		// What the browser says unasked meanwhile is nobody's here.
		if msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return fmt.Errorf("%s: %s", method, msg.Error.Message)
		}
		return json.Unmarshal(msg.Result, result)
	}
}

// target is one of the things a browser has open, a page among them, as
// the browser describes it.
type target struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// browserTabs lists the browser's pages.
func browserTabs(ctx context.Context, dial Dial, host string) ([]BrowserTab, error) {
	var targets []target
	if err := askBrowser(ctx, dial, host, http.MethodGet, "/json/list", &targets); err != nil {
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

// askBrowser asks the browser over HTTP, as the protocol has it for what
// is no page's own, on a connection of its own, and reads its answer
// into result, where there is one: what it says in words is not kept.
func askBrowser(ctx context.Context, dial Dial, host, method, path string, result any) error {
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// The answer is waited for only as long as ctx lasts.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	req, err := http.NewRequest(method, "http://"+host+path, nil)
	if err != nil {
		return err
	}
	req.Close = true
	if err := req.Write(conn); err != nil {
		return err
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the browser answered %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(body, result)
}

// send sends a command. Its answer is not waited for: most commands here
// either have none worth reading or fail by ending the stream.
func (s *Stream) send(method string, params any) error {
	_, err := s.post(method, params, nil)
	return err
}

// post sends a command under an ID of its own, which it returns. The
// browser's answer to it goes to to, where there is one.
func (s *Stream) post(method string, params any, to chan answer) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	if to != nil {
		s.asking.Lock()
		s.asked[s.nextID] = to
		s.asking.Unlock()
	}
	return s.nextID, websocket.JSON.Send(s.ws, map[string]any{"id": s.nextID, "method": method, "params": params})
}

// ask sends a command and reads the browser's answer to it into result,
// for which it waits as long as ctx and the stream last.
func (s *Stream) ask(ctx context.Context, method string, params, result any) error {
	to := make(chan answer, 1)
	id, err := s.post(method, params, to)
	defer func() {
		s.asking.Lock()
		delete(s.asked, id)
		s.asking.Unlock()
	}()
	if err != nil {
		return err
	}
	select {
	case a := <-to:
		if a.Error != nil {
			return fmt.Errorf("%s: %s", method, a.Error.Message)
		}
		return json.Unmarshal(a.Result, result)
	case <-s.done:
		return fmt.Errorf("the stream ended: %w", s.err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Stream) read() {
	defer close(s.done)
	for {
		var msg struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			answer
		}
		if err := websocket.JSON.Receive(s.ws, &msg); err != nil {
			s.err = err
			return
		}
		if msg.ID != 0 {
			// An answer, which is its asker's or nobody's. The channel
			// holds the one answer a command has.
			s.asking.Lock()
			to := s.asked[msg.ID]
			s.asking.Unlock()
			if to != nil {
				to <- msg.answer
			}
			continue
		}
		if msg.Method == "Page.screencastVisibilityChanged" {
			var said struct {
				Visible bool `json:"visible"`
			}
			if json.Unmarshal(msg.Params, &said) == nil {
				s.behind.Store(!said.Visible)
			}
			continue
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
