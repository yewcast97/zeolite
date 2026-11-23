package viewport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// fakeBrowser speaks enough of the DevTools Protocol to stand in for a
// browser: it lists one page, and on that page's socket it sends a
// screencast frame, then another each time one is acknowledged.
type fakeBrowser struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	commands []command
	sent     int
	pages    []*websocket.Conn
	// at is where its pages are in their history, whose entries have
	// the IDs historyEntries.
	at int
	// opensNothing is whether it refuses to open a page.
	opensNothing bool
	// selected is what its pages say is selected in them, and closed
	// the IDs of the pages it was asked to close.
	selected any
	closed   []string
}

// historyEntries are the IDs of the entries of a page's history, first
// to last.
var historyEntries = []int{11, 12, 13}

type command struct {
	ID     int            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

func newFakeBrowser(t *testing.T) *fakeBrowser {
	b := &fakeBrowser{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "127.0.0.1:") {
			t.Errorf("the page list was asked with Host %q", r.Host)
		}
		io.WriteString(w, `[{"id":"worker1","type":"service_worker","title":"a worker","url":"https://example.com/worker.js"},{"id":"page1","type":"page","title":"Example Domain","url":"https://example.com/"},{"id":"page2","type":"page","title":"","url":"about:blank"}]`)
	})
	mux.HandleFunc("/json/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "Using unsafe HTTP verb "+r.Method+" to invoke /json/new. This action supports only PUT verb.", http.StatusMethodNotAllowed)
			return
		}
		// As the browser does: what follows the question mark, unescaped
		// once, is the address.
		address, err := url.PathUnescape(r.URL.RawQuery)
		if err != nil || b.opensNothing {
			http.Error(w, "Could not create new page", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "page3", "type": "page", "title": "", "url": address})
	})
	mux.HandleFunc("/json/close/", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.closed = append(b.closed, strings.TrimPrefix(r.URL.Path, "/json/close/"))
		b.mu.Unlock()
		io.WriteString(w, "Target is closing")
	})
	mux.Handle("/devtools/page/page1", websocket.Handler(b.page))
	mux.Handle("/devtools/page/page2", websocket.Handler(b.page))
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

func (b *fakeBrowser) dial(ctx context.Context) (io.ReadWriteCloser, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", b.server.Listener.Addr().String())
}

func (b *fakeBrowser) frame(ws *websocket.Conn) {
	b.mu.Lock()
	b.sent++
	n := b.sent
	b.mu.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, 64, 36))
	for i := range img.Pix {
		img.Pix[i] = uint8(n * 40)
	}
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	websocket.JSON.Send(ws, map[string]any{
		"method": "Page.screencastFrame",
		"params": map[string]any{
			"data":      base64.StdEncoding.EncodeToString(buf.Bytes()),
			"sessionId": n,
			"metadata":  map[string]any{"deviceWidth": 1280, "deviceHeight": 720},
		},
	})
}

func (b *fakeBrowser) page(ws *websocket.Conn) {
	b.mu.Lock()
	b.pages = append(b.pages, ws)
	b.mu.Unlock()
	for {
		var raw json.RawMessage
		if err := websocket.JSON.Receive(ws, &raw); err != nil {
			return
		}
		var c command
		json.Unmarshal(raw, &c)
		b.mu.Lock()
		b.commands = append(b.commands, c)
		b.mu.Unlock()
		switch c.Method {
		case "Page.startScreencast", "Page.screencastFrameAck":
			b.frame(ws)
		case "Page.getNavigationHistory":
			b.mu.Lock()
			at := b.at
			b.mu.Unlock()
			var entries []map[string]any
			for _, id := range historyEntries {
				entries = append(entries, map[string]any{"id": id})
			}
			websocket.JSON.Send(ws, map[string]any{"id": c.ID, "result": map[string]any{"currentIndex": at, "entries": entries}})
		case "Runtime.evaluate":
			b.mu.Lock()
			selected := b.selected
			b.mu.Unlock()
			websocket.JSON.Send(ws, map[string]any{"id": c.ID, "result": map[string]any{"result": map[string]any{"type": "string", "value": selected}}})
		case "Page.crash":
			websocket.JSON.Send(ws, map[string]any{"id": c.ID, "error": map[string]any{"message": "nothing crashes here"}})
		}
	}
}

// quit closes the pages' sockets, as a browser that exits does.
func (b *fakeBrowser) quit() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ws := range b.pages {
		ws.Close()
	}
}

func (b *fakeBrowser) framesSent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent
}

// count is how many commands of a method the browser got.
func (b *fakeBrowser) count(method string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.commands {
		if c.Method == method {
			n++
		}
	}
	return n
}

func (b *fakeBrowser) command(method string) (command, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.commands) - 1; i >= 0; i-- {
		if b.commands[i].Method == method {
			return b.commands[i], true
		}
	}
	return command{}, false
}

func (b *fakeBrowser) waitCommand(method string) command {
	b.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c, ok := b.command(method); ok {
			return c
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("the browser never got %s", method)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestScreencastIsPacedByItsReader(t *testing.T) {
	b := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := OpenCDP(ctx, b.dial, 9222, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if start := b.waitCommand("Page.startScreencast"); start.Params["format"] != "jpeg" || start.Params["maxWidth"] != float64(MaxFrameWidth) {
		t.Errorf("screencast started with %v", start.Params)
	}

	f, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Image.Bounds().Dx() != 64 || f.PageWidth != 1280 || f.PageHeight != 720 {
		t.Errorf("frame %v, page %vx%v", f.Image.Bounds(), f.PageWidth, f.PageHeight)
	}
	// A JPEG of this kind decodes to planes the dashboard can show as
	// they are.
	if ycc, ok := f.Image.(*image.YCbCr); !ok || ycc.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		t.Errorf("the frame decoded to %T", f.Image)
	}

	// Until the frame is said to have been shown, the browser is not
	// asked for another.
	time.Sleep(200 * time.Millisecond)
	if n := b.framesSent(); n != 1 {
		t.Fatalf("%d frames were sent before the first was acknowledged", n)
	}
	if _, acked := b.command("Page.screencastFrameAck"); acked {
		t.Fatal("the first frame was acknowledged before Next was called again")
	}

	if _, err := s.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if ack := b.waitCommand("Page.screencastFrameAck"); ack.Params["sessionId"] != float64(1) {
		t.Errorf("acknowledged %v", ack.Params)
	}
	if n := b.framesSent(); n != 2 {
		t.Errorf("%d frames sent after one acknowledgement", n)
	}
}

func TestInputReachesTheBrowser(t *testing.T) {
	b := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := OpenCDP(ctx, b.dial, 9222, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Pointer(PointerPressed, 640, 360, 0, 1, Shift)
	if c := b.waitCommand("Input.dispatchMouseEvent"); c.Params["type"] != "mousePressed" || c.Params["x"] != float64(640) || c.Params["button"] != "left" || c.Params["modifiers"] != float64(Shift) {
		t.Errorf("pointer press sent as %v", c.Params)
	}
	s.Pointer(PointerMoved, 10, 20, -1, 0, 0)
	time.Sleep(50 * time.Millisecond)
	if c := b.waitCommand("Input.dispatchMouseEvent"); c.Params["type"] != "mouseMoved" || c.Params["button"] != "none" {
		t.Errorf("pointer move sent as %v", c.Params)
	}
	s.Scroll(100, 100, 0, 120, 0)
	time.Sleep(50 * time.Millisecond)
	if c := b.waitCommand("Input.dispatchMouseEvent"); c.Params["type"] != "mouseWheel" || c.Params["deltaY"] != float64(120) {
		t.Errorf("scroll sent as %v", c.Params)
	}
	s.Text("你好")
	if c := b.waitCommand("Input.insertText"); c.Params["text"] != "你好" {
		t.Errorf("text sent as %v", c.Params)
	}
	s.PressKey(KeyBackspace, true, 0)
	if c := b.waitCommand("Input.dispatchKeyEvent"); c.Params["type"] != "rawKeyDown" || c.Params["key"] != "Backspace" || c.Params["windowsVirtualKeyCode"] != float64(8) {
		t.Errorf("key sent as %v", c.Params)
	}
	s.PressKey(KeyEnter, true, 0)
	time.Sleep(50 * time.Millisecond)
	if c := b.waitCommand("Input.dispatchKeyEvent"); c.Params["type"] != "char" || c.Params["text"] != "\r" {
		t.Errorf("Enter did not type its character: %v", c.Params)
	}
}

// What an input method composes is shown in the page as it is composed,
// its caret counted as the browser counts, and ends with an empty text.
func TestCompositionReachesTheBrowser(t *testing.T) {
	b := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := OpenCDP(ctx, b.dial, 9222, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The caret is after two characters, the first of which the browser
	// counts as two units.
	s.Compose("😀ni hao", 2)
	if c := b.waitCommand("Input.imeSetComposition"); c.Params["text"] != "😀ni hao" || c.Params["selectionStart"] != float64(3) || c.Params["selectionEnd"] != float64(3) {
		t.Errorf("a composition sent as %v", c.Params)
	}
	// A caret beyond the text is at its end.
	s.Compose("ni", 9)
	time.Sleep(50 * time.Millisecond)
	if c := b.waitCommand("Input.imeSetComposition"); c.Params["text"] != "ni" || c.Params["selectionStart"] != float64(2) {
		t.Errorf("a composition whose caret is beyond it sent as %v", c.Params)
	}
	s.Compose("", 0)
	time.Sleep(50 * time.Millisecond)
	if c := b.waitCommand("Input.imeSetComposition"); c.Params["text"] != "" || c.Params["selectionStart"] != float64(0) {
		t.Errorf("the end of a composition sent as %v", c.Params)
	}
}

// The commands of an Edit menu reach the page as editing commands, on a
// key event that names no key. What is selected is the page's answer,
// taken up to a length and never as anything but text. A stream closes
// the Browser Tab it shows, and no other.
func TestEditingSelectionAndClosing(t *testing.T) {
	b := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := OpenCDP(ctx, b.dial, 9222, "page2")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Edit(EditSelectAll)
	b.waitCommand("Input.dispatchKeyEvent")
	time.Sleep(50 * time.Millisecond)
	b.mu.Lock()
	var keys []command
	for _, c := range b.commands {
		if c.Method == "Input.dispatchKeyEvent" {
			keys = append(keys, c)
		}
	}
	b.mu.Unlock()
	if len(keys) != 2 || keys[0].Params["type"] != "rawKeyDown" || !slices.Equal(keys[0].Params["commands"].([]any), []any{"selectAll"}) || keys[0].Params["key"] != nil || keys[1].Params["type"] != "keyUp" || keys[1].Params["commands"] != nil {
		t.Errorf("Select All sent as %v", keys)
	}

	selects := func(what any) string {
		t.Helper()
		b.mu.Lock()
		b.selected = what
		b.mu.Unlock()
		text, err := s.Selection(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	if got := selects("你好, page"); got != "你好, page" {
		t.Errorf("the selection is %q", got)
	}
	if c := b.waitCommand("Runtime.evaluate"); c.Params["returnByValue"] != true || !strings.Contains(c.Params["expression"].(string), "getSelection") {
		t.Errorf("the selection was asked for with %v", c.Params)
	}
	// A page that answers with something else than text selected none.
	if got := selects(map[string]any{"not": "text"}); got != "" {
		t.Errorf("a selection that is no text is %q", got)
	}
	// A long one is cut, and never in the middle of a character.
	long := strings.Repeat("好", maxSelection/3+10)
	if got := selects(long); len(got) > maxSelection || len(got) < maxSelection-3 || !strings.HasPrefix(long, got) {
		t.Errorf("a selection of %d bytes was taken as %d", len(long), len(got))
	}

	if err := s.CloseTab(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(b.closed, []string{"page2"}) {
		t.Errorf("the browser was asked to close %q, want the Browser Tab the stream shows", b.closed)
	}
}

// Back and Forward send the Browser Tab shown to the entry before and
// after the one it is at in its own history, which the browser is asked
// for; at an end of it nothing is sent. An answer is its asker's alone,
// and a stream that ends answers whoever still waits.
func TestBackAndForward(t *testing.T) {
	b := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := OpenCDP(ctx, b.dial, 9222, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := func(i int) {
		b.mu.Lock()
		b.at = i
		b.mu.Unlock()
	}

	at(1)
	if err := s.Back(ctx); err != nil {
		t.Fatal(err)
	}
	if c := b.waitCommand("Page.navigateToHistoryEntry"); c.Params["entryId"] != float64(historyEntries[0]) {
		t.Errorf("Back from the second entry sent the Browser Tab to %v", c.Params)
	}
	if err := s.Forward(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if c := b.waitCommand("Page.navigateToHistoryEntry"); c.Params["entryId"] != float64(historyEntries[2]) {
		t.Errorf("Forward from the second entry sent the Browser Tab to %v", c.Params)
	}

	// At the first entry Back sends nothing, and Forward at the last.
	sent := b.count("Page.navigateToHistoryEntry")
	at(0)
	if err := s.Back(ctx); err != nil {
		t.Fatal(err)
	}
	at(len(historyEntries) - 1)
	if err := s.Forward(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := b.count("Page.navigateToHistoryEntry"); n != sent {
		t.Errorf("at the ends of its history the Browser Tab was sent somewhere %d times", n-sent)
	}

	// What the browser refuses is an error, in the browser's words.
	var none struct{}
	if err := s.ask(ctx, "Page.crash", map[string]any{}, &none); err == nil || !strings.Contains(err.Error(), "nothing crashes here") {
		t.Errorf("a command the browser refused gave %v", err)
	}
	// An answer that never comes is waited for no longer than ctx lasts,
	// or the stream.
	brief, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if err := s.ask(brief, "Page.enable", map[string]any{}, &none); err != context.DeadlineExceeded {
		t.Errorf("an answer that never came gave %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- s.ask(ctx, "Page.enable", map[string]any{}, &none) }()
	time.Sleep(50 * time.Millisecond)
	b.quit()
	select {
	case err := <-waited:
		if err == nil || !strings.Contains(err.Error(), "the stream ended") {
			t.Errorf("the end of the stream answered %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("an asker outlived its stream")
	}
	if len(s.asked) != 0 {
		t.Errorf("%d answers are still waited for", len(s.asked))
	}
}

func TestStreamEnds(t *testing.T) {
	b := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := OpenCDP(ctx, b.dial, 9222, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(ctx); err != nil {
		t.Fatal(err)
	}
	b.quit()
	if _, err := s.Next(ctx); err == nil {
		t.Error("a stream whose browser went away gave a frame")
	}
	s.Close()

	// A browser with no page.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `[]`) }))
	defer empty.Close()
	_, err = OpenCDP(ctx, func(ctx context.Context) (io.ReadWriteCloser, error) {
		return net.Dial("tcp", empty.Listener.Addr().String())
	}, 9222, "")
	if err == nil || !strings.Contains(err.Error(), "no page") {
		t.Errorf("a browser without pages gave %v", err)
	}
}

// The browser's pages are its Browser Tabs, in its order, with what each
// calls itself and where it is. A stream shows the first, or the one it
// was asked for, which is brought to the front; and it sends the one it
// shows to an address.
func TestBrowserTabs(t *testing.T) {
	b := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := OpenCDP(ctx, b.dial, 9222, "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	b.waitCommand("Page.startScreencast")
	if first.Tab() != "page1" {
		t.Errorf("a stream that was asked for no Browser Tab shows %q, want the first", first.Tab())
	}
	if _, fronted := b.command("Page.bringToFront"); fronted {
		t.Error("a stream that was asked for no Browser Tab brought one to the front")
	}
	tabs, err := first.Tabs(ctx)
	want := []BrowserTab{{ID: "page1", Title: "Example Domain", Address: "https://example.com/"}, {ID: "page2", Address: "about:blank"}}
	if err != nil || !slices.Equal(tabs, want) {
		t.Fatalf("the Browser Tabs are %+v (%v), want %+v", tabs, err, want)
	}

	second, err := OpenCDP(ctx, b.dial, 9222, "page2")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	b.waitCommand("Page.bringToFront")
	if second.Tab() != "page2" {
		t.Errorf("a stream asked for page2 shows %q", second.Tab())
	}
	second.Navigate("https://example.org/next")
	if c := b.waitCommand("Page.navigate"); c.Params["url"] != "https://example.org/next" {
		t.Errorf("the Browser Tab was sent to %v", c.Params)
	}

	// An ID the browser does not list opens nothing: it is never put in
	// a path.
	for _, gone := range []string{"page3", "../../json/close/page1", "worker1"} {
		if s, err := OpenCDP(ctx, b.dial, 9222, gone); err == nil {
			s.Close()
			t.Errorf("a stream was opened on %q", gone)
		}
	}
}

// A new Browser Tab is opened at an address as it is, with the escapes
// of its own, though the browser unescapes what it is asked with; and a
// browser that opens none is an error, in its words.
func TestNewTab(t *testing.T) {
	b := newFakeBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, address := range []string{
		"https://example.com/",
		"https://accounts.example.com/o/auth?redirect_uri=http%3A%2F%2Flocalhost%3A8085%2Fcallback&scope=a%20b+c&x=%2B#state%41",
		"https://example.com/a b?q=你好&r=100%",
	} {
		tab, err := NewTab(ctx, b.dial, 9222, address)
		if err != nil || tab.ID != "page3" || tab.Address != address {
			t.Errorf("a Browser Tab asked for at %q is %+v (%v)", address, tab, err)
		}
	}
	b.opensNothing = true
	if tab, err := NewTab(ctx, b.dial, 9222, "https://example.com/"); err == nil || !strings.Contains(err.Error(), "Could not create new page") {
		t.Errorf("a browser that opens nothing gave %+v (%v)", tab, err)
	}
}

func TestAddress(t *testing.T) {
	for typed, want := range map[string]string{
		"":                             "",
		"  ":                           "",
		"example.com":                  "https://example.com",
		" example.com/a?b=c ":          "https://example.com/a?b=c",
		"example.com:8443/x":           "https://example.com:8443/x",
		"http://example.com":           "http://example.com",
		"https://example.com/":         "https://example.com/",
		"file:///workspace/index.html": "file:///workspace/index.html",
		"about:blank":                  "about:blank",
		"localhost:8000":               "http://localhost:8000",
		"localhost":                    "http://localhost",
		"127.0.0.1:8000/site":          "http://127.0.0.1:8000/site",
		"[::1]:3000":                   "http://[::1]:3000",
		"198.51.100.4":                 "https://198.51.100.4",
	} {
		if got := Address(typed); got != want {
			t.Errorf("the address of %q is %q, want %q", typed, got, want)
		}
	}
}
