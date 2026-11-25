//go:build integration

package integration

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"zeolite/internal/pack"
	"zeolite/internal/sandbox"
	"zeolite/internal/viewport"
)

// clickPage is a page that reports where it was clicked, what was typed
// into it and what an input method composed there, to the server it
// came from.
const clickPage = `<!doctype html><title>click target</title>
<body style="margin:0;background:#246"><input id=i autofocus style="position:absolute;left:40px;top:300px">
<script>
document.addEventListener("click", e => fetch("/clicked/" + e.clientX + "/" + e.clientY));
document.getElementById("i").addEventListener("input", e => fetch("/typed/" + encodeURIComponent(e.target.value)));
document.getElementById("i").addEventListener("compositionupdate", e => fetch("/composed/" + encodeURIComponent(e.data)));
</script>`

func TestWebTestBrowserViewport(t *testing.T) {
	catalog, err := pack.NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	enabled := catalog.Enabled(sandbox.Config{Packs: []string{"web-test-browser"}})
	if len(enabled) != 1 {
		t.Fatal("no web-test-browser pack")
	}
	browser := enabled[0]
	cfg := lockedConfig("browser", "Browser")
	cfg.Packs = []string{browser.Name}
	start(t, cfg, []pack.Manifest{browser}, nil)

	// The supervisor reports the pack's daemon ready once Chromium
	// accepts connections on its debugging port.
	waitFor(t, 60*time.Second, "Chromium to be ready", func() bool {
		w, _ := updates.workstationOf(cfg.ID)
		return len(w.Daemons) == 1 && w.Daemons[0].Pack == browser.Name && w.Daemons[0].Ready
	})
	// The workstation says how much memory it has in use, and how much it
	// may have, which is what its Setup gives it: the browser's control is
	// dimmed by these, and must not be for want of them.
	limit := uint64(cfg.Resources.MemoryMB) << 20
	waitFor(t, 30*time.Second, "the workstation to say what memory it has in use", func() bool {
		w, _ := updates.workstationOf(cfg.ID)
		return w.MemoryLimit == limit && w.MemoryUsed > 0 && w.MemoryUsed < limit
	})
	if w, _ := updates.workstationOf(cfg.ID); true {
		t.Logf("with Chromium ready the workstation has %d MB in use of %d MB", w.MemoryUsed>>20, w.MemoryLimit>>20)
	}
	if out, _ := inSandbox(cfg.ID, `cat "$OPENCODE_CONFIG" ~/.claude.json ~/.codex/config.toml`); strings.Count(out, "browser") < 3 || strings.Count(out, "mcp-bridge") < 3 {
		t.Errorf("the agents' MCP registries do not start the pack's server through the bridge:\n%s", out)
	}

	// A page served in the sandbox, in place of the blank one.
	script := `mkdir -p /workspace/site && cat > /workspace/site/index.html <<'PAGE'
` + clickPage + `
PAGE
cd /workspace/site && (python3 -m http.server 8000 --bind 127.0.0.1 > /workspace/server.log 2>&1 &)
sleep 1
old=$(curl -s http://127.0.0.1:9222/json/list | python3 -c 'import json,sys; print(" ".join(t["id"] for t in json.load(sys.stdin) if t["type"]=="page"))')
curl -s -X PUT 'http://127.0.0.1:9222/json/new?http://127.0.0.1:8000/' > /dev/null
for id in $old; do curl -s "http://127.0.0.1:9222/json/close/$id" > /dev/null; done
sleep 1; curl -s http://127.0.0.1:9222/json/list | python3 -c 'import json,sys; print([t["url"] for t in json.load(sys.stdin) if t["type"]=="page"])'`
	if out, code := inSandbox(cfg.ID, script); code != 0 || !strings.Contains(out, "http://127.0.0.1:8000/") {
		t.Fatalf("opening the test page: %s", out)
	}

	// The browser calls itself what it would with a window, by its own
	// version: nothing in its User-Agent says that it has none.
	if out, _ := inSandbox(cfg.ID, `curl -s http://127.0.0.1:9222/json/version`); !strings.Contains(out, "(KHTML, like Gecko) Chrome/") || strings.Contains(out, "Headless") {
		t.Errorf("the browser calls itself:\n%s", out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		return orch.DialWorkstation(ctx, cfg.ID, browser.ViewportStream.Port)
	}
	stream, err := viewport.OpenCDP(ctx, dial, browser.ViewportStream.Port, "")
	if err != nil {
		t.Fatalf("opening the Viewport Stream: %v", err)
	}
	defer stream.Close()
	// listed are the browser's Browser Tabs as it lists them now.
	listed := func() ([]viewport.BrowserTab, error) {
		return viewport.Tabs(ctx, dial, browser.ViewportStream.Port)
	}

	frame, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("the first frame: %v", err)
	}
	if b := frame.Image.Bounds(); b.Dx() < 320 || b.Dx() > viewport.MaxFrameWidth || b.Dy() > viewport.MaxFrameHeight {
		t.Errorf("a frame of %v", b)
	}
	if frame.PageWidth < 320 || frame.PageHeight < 200 {
		t.Errorf("the page measures %vx%v", frame.PageWidth, frame.PageHeight)
	}
	// The page is a solid colour: the frame shows the page, not a blank.
	r, g, b, _ := frame.Image.At(frame.Image.Bounds().Dx()-10, 10).RGBA()
	if !(b > r && b > g) {
		t.Errorf("the frame's corner is rgb(%d,%d,%d), not the page's blue", r>>8, g>>8, b>>8)
	}

	// A click and typed text reach the page where they were aimed.
	stream.Pointer(viewport.PointerPressed, 200, 120, 0, 1, 0)
	stream.Pointer(viewport.PointerReleased, 200, 120, 0, 1, 0)
	stream.Pointer(viewport.PointerPressed, 60, 310, 0, 1, 0)
	stream.Pointer(viewport.PointerReleased, 60, 310, 0, 1, 0)
	stream.Text("你好 viewport")
	waitFor(t, 10*time.Second, "the page to report the click and the text", func() bool {
		log, _ := inSandbox(cfg.ID, "cat /workspace/server.log")
		return strings.Contains(log, "GET /clicked/200/120") && strings.Contains(log, "GET /typed/%E4%BD%A0%E5%A5%BD%20viewport")
	})

	// What an input method composes shows in the field as it is
	// composed; its end leaves nothing of it, and what is committed
	// stands after what was typed before.
	stream.Compose("ni hao", 6)
	waitFor(t, 10*time.Second, "the page to report the composition", func() bool {
		log, _ := inSandbox(cfg.ID, "cat /workspace/server.log")
		return strings.Contains(log, "GET /composed/ni%20hao ") && strings.Contains(log, "GET /typed/%E4%BD%A0%E5%A5%BD%20viewportni%20hao ")
	})
	stream.Compose("", 0)
	stream.Text("好")
	waitFor(t, 10*time.Second, "the page to report what was committed", func() bool {
		log, _ := inSandbox(cfg.ID, "cat /workspace/server.log")
		return strings.Contains(log, "GET /typed/%E4%BD%A0%E5%A5%BD%20viewport%E5%A5%BD ")
	})

	// Select All selects what the field holds, and the selection is
	// that text; a cut leaves the field empty.
	stream.Edit(viewport.EditSelectAll)
	waitFor(t, 10*time.Second, "the field's text to be selected", func() bool {
		text, err := stream.Selection(ctx)
		return err == nil && text == "你好 viewport好"
	})
	stream.Edit(viewport.EditCut)
	waitFor(t, 10*time.Second, "the page to report the cut", func() bool {
		log, _ := inSandbox(cfg.ID, "cat /workspace/server.log")
		return strings.Contains(log, "GET /typed/ ")
	})
	if text, err := stream.Selection(ctx); err != nil || text != "" {
		t.Errorf("after a cut the selection is %q (%v)", text, err)
	}

	// More frames come as they are asked for.
	for i := 0; i < 3; i++ {
		stream.Pointer(viewport.PointerMoved, float64(100+i*40), 200, -1, 0, 0)
		if _, err := stream.Next(ctx); err != nil {
			t.Fatalf("frame %d: %v", i+2, err)
		}
	}

	// The browser's one Browser Tab is the page, by its own title and
	// its address, and the stream shows it.
	tabs, err := listed()
	if err != nil || len(tabs) != 1 || tabs[0].Title != "click target" || tabs[0].Address != "http://127.0.0.1:8000/" || tabs[0].ID != stream.Tab() {
		t.Fatalf("the Browser Tabs are %+v (%v), and the stream shows %q", tabs, err, stream.Tab())
	}

	// An address typed without its scheme is the sandbox's own loopback
	// over HTTP, and the Browser Tab shown goes there: a red page.
	if out, code := inSandbox(cfg.ID, `cd /workspace/site && echo '<!doctype html><title>second</title><body style="margin:0;background:#c22">' > second.html && echo '<!doctype html><title>third</title><body style="margin:0;background:#2a4">' > third.html`); code != 0 {
		t.Fatal(out)
	}
	if err := stream.Navigate(viewport.Address("127.0.0.1:8000/second.html")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the browser to list the address the Browser Tab was sent to", func() bool {
		tabs, err := listed()
		return err == nil && len(tabs) == 1 && tabs[0].Address == "http://127.0.0.1:8000/second.html" && tabs[0].Title == "second"
	})
	// corner is the colour of a frame's corner, once frames of that
	// colour come.
	corner := func(s *viewport.Stream, what string, is func(r, g, b uint32) bool) {
		t.Helper()
		var r, g, b uint32
		for range 20 {
			frame, err := s.Next(ctx)
			if err != nil {
				t.Fatalf("a frame of %s: %v", what, err)
			}
			if r, g, b, _ = frame.Image.At(frame.Image.Bounds().Dx()-10, 10).RGBA(); is(r, g, b) {
				return
			}
			s.Pointer(viewport.PointerMoved, 300, 200, -1, 0, 0)
		}
		t.Errorf("the frames of %s have a corner of rgb(%d,%d,%d)", what, r>>8, g>>8, b>>8)
	}
	corner(stream, "the page the Browser Tab was sent to", func(r, g, b uint32) bool { return r > g && r > b })

	// Back is the page it came from, and Forward the red page again;
	// at the end of its history Forward leaves the Browser Tab there.
	lists := func(what, address, title string) {
		t.Helper()
		waitFor(t, 10*time.Second, what, func() bool {
			tabs, err := listed()
			return err == nil && len(tabs) == 1 && tabs[0].Address == address && tabs[0].Title == title
		})
	}
	if err := stream.Back(ctx); err != nil {
		t.Fatalf("Back: %v", err)
	}
	lists("the browser to list the page before", "http://127.0.0.1:8000/", "click target")
	corner(stream, "the page before", func(r, g, b uint32) bool { return b > r && b > g })
	if err := stream.Back(ctx); err != nil {
		t.Fatalf("Back at the start of the history: %v", err)
	}
	if err := stream.Forward(ctx); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	lists("the browser to list the page after", "http://127.0.0.1:8000/second.html", "second")
	if err := stream.Forward(ctx); err != nil {
		t.Fatalf("Forward at the end of the history: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	lists("the Browser Tab to stay at the end of its history", "http://127.0.0.1:8000/second.html", "second")
	corner(stream, "the page after", func(r, g, b uint32) bool { return r > g && r > b })

	// A second Browser Tab, a green page, opened as a link clicked in a
	// Pane is, at an address with escapes of its own: the browser has it
	// at that address, as it is. It lists both, and a stream that is
	// asked for one shows that one, while the first stream's Browser Tab
	// stays what it was.
	const linked = "http://127.0.0.1:8000/third.html?redirect_uri=http%3A%2F%2Flocalhost%3A8085%2Fcallback&scope=a%20b+c#state"
	opened, err := viewport.NewTab(ctx, dial, browser.ViewportStream.Port, linked)
	if err != nil || opened == "" {
		t.Fatalf("the Browser Tab opened at %s is %q (%v)", linked, opened, err)
	}
	var third, second string
	waitFor(t, 10*time.Second, "the browser to list its second Browser Tab", func() bool {
		tabs, err := listed()
		if err != nil || len(tabs) != 2 {
			return false
		}
		third, second = "", ""
		for _, tab := range tabs {
			switch tab.Title {
			case "third":
				if tab.Address == linked {
					third = tab.ID
				}
			case "second":
				second = tab.ID
			}
		}
		return third == opened && second == stream.Tab()
	})
	other, err := viewport.OpenCDP(ctx, dial, browser.ViewportStream.Port, third)
	if err != nil {
		t.Fatalf("opening the Viewport Stream of the second Browser Tab: %v", err)
	}
	defer other.Close()
	if other.Tab() != third {
		t.Errorf("the stream asked for %s shows %s", third, other.Tab())
	}
	corner(other, "the second Browser Tab", func(r, g, b uint32) bool { return g > r && g > b })
	// And back: the first one, chosen again, is the red page still.
	back, err := viewport.OpenCDP(ctx, dial, browser.ViewportStream.Port, second)
	if err != nil {
		t.Fatalf("opening the Viewport Stream of the first Browser Tab again: %v", err)
	}
	defer back.Close()
	corner(back, "the first Browser Tab, chosen again", func(r, g, b uint32) bool { return r > g && r > b })
	// A stream closes the Browser Tab it shows: the browser lists the
	// other alone, whose streams go on. The tab that was closed had a
	// window of its own, so that nothing is drawn anew for its going:
	// the one that is left is sent to another page, and shows it.
	if err := other.CloseTab(ctx); err != nil {
		t.Fatalf("closing the second Browser Tab: %v", err)
	}
	waitFor(t, 10*time.Second, "the browser to list one Browser Tab again", func() bool {
		tabs, err := listed()
		return err == nil && len(tabs) == 1 && tabs[0].ID == second
	})
	if err := back.Navigate("http://127.0.0.1:8000/third.html"); err != nil {
		t.Fatal(err)
	}
	corner(back, "the first Browser Tab, sent to another page once the second was closed", func(r, g, b uint32) bool { return g > r && g > b })
	if s, err := viewport.OpenCDP(ctx, dial, browser.ViewportStream.Port, "no-such-tab"); err == nil {
		s.Close()
		t.Error("a stream was opened on a Browser Tab the browser does not have")
	}
}

// movingPage is a page that changes colour ten times a second, so that a
// Browser Tab that draws gives frames without being touched, and that
// reports what is typed into it to the server it came from, under the
// name it was opened by.
const movingPage = `<!doctype html><title>moving</title>
<body style="margin:0"><input id=i autofocus>
<script>
let n = 0;
setInterval(() => { n++; document.body.style.background = n % 2 ? "#c22" : "#2a4"; }, 100);
document.getElementById("i").addEventListener("input", e => fetch("/typed/" + location.search.slice(1) + "/" + encodeURIComponent(e.target.value)));
</script>`

// Several Panes show the browser at once, each a Browser Tab of its own,
// and each must draw while the others do. A browser draws the front tab
// of each of its windows and no other: so the Browser Tabs the app opens
// each have a window of their own, where they draw together, take what
// is typed without ever having been brought forward, and have pages of
// one size, as wide as the browser's first. A Browser Tab that is opened with no
// window of its own, as the pack's own MCP server opens one for an
// agent, goes to the front of the window last brought forward, and the
// tab that was in front there stands behind it: its stream says so and
// gives no frame, until it is brought forward. The app's own Browser
// Tabs are untouched by that, as long as none of them was brought
// forward: which is why a stream brings none forward unasked.
func TestBrowserTabsDrawAtOnce(t *testing.T) {
	catalog, err := pack.NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	enabled := catalog.Enabled(sandbox.Config{Packs: []string{"web-test-browser"}})
	if len(enabled) != 1 {
		t.Fatal("no web-test-browser pack")
	}
	browser := enabled[0]
	cfg := lockedConfig("tabs", "Tabs")
	cfg.Packs = []string{browser.Name}
	start(t, cfg, []pack.Manifest{browser}, nil)
	waitFor(t, 60*time.Second, "Chromium to be ready", func() bool {
		w, _ := updates.workstationOf(cfg.ID)
		return len(w.Daemons) == 1 && w.Daemons[0].Pack == browser.Name && w.Daemons[0].Ready
	})
	script := `mkdir -p /workspace/site && cat > /workspace/site/index.html <<'PAGE'
` + movingPage + `
PAGE
cd /workspace/site && (python3 -m http.server 8000 --bind 127.0.0.1 > /workspace/server.log 2>&1 &)
sleep 1; curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8000/`
	if out, code := inSandbox(cfg.ID, script); code != 0 || !strings.HasSuffix(trimmed(out), "200") {
		t.Fatalf("serving the test page: %s", out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	port := browser.ViewportStream.Port
	dial := func(ctx context.Context) (io.ReadWriteCloser, error) {
		return orch.DialWorkstation(ctx, cfg.ID, port)
	}
	const page = "http://127.0.0.1:8000/?"
	open := func(name string) *viewport.Stream {
		t.Helper()
		tab, err := viewport.NewTab(ctx, dial, port, page+name)
		if err != nil {
			t.Fatalf("opening the Browser Tab %s: %v", name, err)
		}
		s, err := viewport.OpenCDP(ctx, dial, port, tab)
		if err != nil {
			t.Fatalf("opening the stream of the Browser Tab %s: %v", name, err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	// frames is how many frames each stream gives in a time, all of them
	// read together, and the size of the page of the last of each.
	type size struct{ width, height float64 }
	frames := func(d time.Duration, streams ...*viewport.Stream) ([]int, []size) {
		t.Helper()
		counts, sizes := make([]int, len(streams)), make([]size, len(streams))
		until, stop := context.WithTimeout(ctx, d)
		defer stop()
		done := make(chan struct{})
		for i, s := range streams {
			go func() {
				defer func() { done <- struct{}{} }()
				for {
					frame, err := s.Next(until)
					if err != nil {
						return
					}
					counts[i]++
					sizes[i] = size{frame.PageWidth, frame.PageHeight}
				}
			}()
		}
		for range streams {
			<-done
		}
		return counts, sizes
	}
	behind := func(what string, s *viewport.Stream, want bool) {
		t.Helper()
		waitFor(t, 10*time.Second, what, func() bool { return s.Behind() == want })
	}

	// The browser's first Browser Tab, which the pack opened, in the
	// browser's first window.
	first, err := viewport.OpenCDP(ctx, dial, port, "")
	if err != nil {
		t.Fatalf("opening the stream of the browser's first Browser Tab: %v", err)
	}
	defer first.Close()
	if err := first.Navigate(page + "first"); err != nil {
		t.Fatal(err)
	}

	// Two Browser Tabs the app opens: both draw, with the first, at once.
	// Their pages are of one size, and as wide as the first's, which is
	// lower by what the browser says over it of how it was started.
	a, b := open("a"), open("b")
	counts, sizes := frames(3*time.Second, first, a, b)
	t.Logf("in three seconds the browser's first Browser Tab gave %d frames, and the two the app opened %d and %d; their pages are %v, %v and %v", counts[0], counts[1], counts[2], sizes[0], sizes[1], sizes[2])
	for i, name := range []string{"the browser's first Browser Tab", "the first Browser Tab the app opened", "the second Browser Tab the app opened"} {
		if counts[i] < 10 {
			t.Errorf("%s gave %d frames in three seconds, while two others drew, of a page that changes ten times a second", name, counts[i])
		}
		if sizes[i].width != viewport.MaxFrameWidth || sizes[i].height < 400 || sizes[i].height > viewport.MaxFrameHeight {
			t.Errorf("the page of %s measures %v", name, sizes[i])
		}
	}
	if sizes[1] != sizes[2] {
		t.Errorf("the pages of the two Browser Tabs the app opened measure %v and %v", sizes[1], sizes[2])
	}
	if first.Behind() || a.Behind() || b.Behind() {
		t.Errorf("of three Browser Tabs that draw, the browser says these are behind: %v, %v, %v", first.Behind(), a.Behind(), b.Behind())
	}

	// What is typed reaches each, though neither was ever brought
	// forward.
	a.Text("to-a")
	b.Text("to-b")
	waitFor(t, 10*time.Second, "what was typed to reach both Browser Tabs", func() bool {
		log, _ := inSandbox(cfg.ID, "cat /workspace/server.log")
		return strings.Contains(log, "GET /typed/a/to-a ") && strings.Contains(log, "GET /typed/b/to-b ")
	})

	// A Browser Tab opened with no window of its own, as the browser's
	// own way of opening one has it, goes to the front of the browser's
	// first window: the tab that was in front there is behind it, and
	// gives no frame, while the app's two draw on.
	plain := `curl -s -X PUT 'http://127.0.0.1:9222/json/new?http://127.0.0.1:8000/?plain' | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'`
	out, code := inSandbox(cfg.ID, plain)
	if code != 0 || trimmed(out) == "" {
		t.Fatalf("opening a Browser Tab with no window of its own: %s", out)
	}
	behind("the browser's first Browser Tab to be behind the new one", first, true)
	counts, _ = frames(2*time.Second, first, a, b)
	if counts[0] > 1 || counts[1] < 7 || counts[2] < 7 {
		t.Errorf("with a Browser Tab in front of the browser's first, the first gave %d frames in two seconds and the app's two %d and %d", counts[0], counts[1], counts[2])
	}
	if a.Behind() || b.Behind() {
		t.Errorf("a Browser Tab opened in the browser's first window put the app's own behind: %v, %v", a.Behind(), b.Behind())
	}
	// A stream opened on a tab that is behind says so at once, and
	// brings nothing forward.
	late, err := viewport.OpenCDP(ctx, dial, port, first.Tab())
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	behind("a stream opened on a Browser Tab that is behind to say so", late, true)
	// Brought forward, it draws again.
	if err := first.Front(); err != nil {
		t.Fatal(err)
	}
	behind("the browser's first Browser Tab to be in front again", first, false)
	if counts, _ = frames(2*time.Second, first); counts[0] < 7 {
		t.Errorf("brought forward, the browser's first Browser Tab gave %d frames in two seconds", counts[0])
	}

	// The pack's own MCP server opens a Browser Tab for an agent the same
	// way: in the window last brought forward, which is not one of the
	// app's two, and they draw on.
	calls := `'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}' '{"jsonrpc":"2.0","method":"notifications/initialized"}' '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"browser_tabs","arguments":{"action":"new"}}}'`
	agent := make(chan string, 1)
	go func() {
		out, _ := inSandbox(cfg.ID, `(printf '%s\n' `+calls+`; sleep 6) | timeout 30 playwright-mcp --cdp-endpoint http://127.0.0.1:9222 2>&1 | tail -c 600`)
		agent <- out
	}()
	behind("the browser's first Browser Tab to be behind the agent's", first, true)
	counts, _ = frames(2*time.Second, a, b)
	if counts[0] < 7 || counts[1] < 7 || a.Behind() || b.Behind() {
		t.Errorf("with a Browser Tab the pack's MCP server opened, the app's two gave %d and %d frames in two seconds, and are behind: %v, %v", counts[0], counts[1], a.Behind(), b.Behind())
	}
	if said := <-agent; !strings.Contains(said, "(current)") {
		t.Errorf("the pack's MCP server did not open a Browser Tab: %s", said)
	}

	// What must not be done unasked: a Browser Tab of the app's that is
	// brought forward takes the window that the next tab with no window
	// of its own comes to the front of, which then stands before it.
	if err := a.Front(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if out, code := inSandbox(cfg.ID, plain); code != 0 || trimmed(out) == "" {
		t.Fatalf("opening a Browser Tab with no window of its own: %s", out)
	}
	behind("a Browser Tab that was brought forward to be behind the next one opened", a, true)
	if b.Behind() {
		t.Error("the Browser Tab that was not brought forward is behind too")
	}
}
