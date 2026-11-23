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
	tabs, err := stream.Tabs(ctx)
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
		tabs, err := stream.Tabs(ctx)
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
	listed := func(what, address, title string) {
		t.Helper()
		waitFor(t, 10*time.Second, what, func() bool {
			tabs, err := stream.Tabs(ctx)
			return err == nil && len(tabs) == 1 && tabs[0].Address == address && tabs[0].Title == title
		})
	}
	if err := stream.Back(ctx); err != nil {
		t.Fatalf("Back: %v", err)
	}
	listed("the browser to list the page before", "http://127.0.0.1:8000/", "click target")
	corner(stream, "the page before", func(r, g, b uint32) bool { return b > r && b > g })
	if err := stream.Back(ctx); err != nil {
		t.Fatalf("Back at the start of the history: %v", err)
	}
	if err := stream.Forward(ctx); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	listed("the browser to list the page after", "http://127.0.0.1:8000/second.html", "second")
	if err := stream.Forward(ctx); err != nil {
		t.Fatalf("Forward at the end of the history: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	listed("the Browser Tab to stay at the end of its history", "http://127.0.0.1:8000/second.html", "second")
	corner(stream, "the page after", func(r, g, b uint32) bool { return r > g && r > b })

	// A second Browser Tab, a green page, opened as a link clicked in a
	// Pane is, at an address with escapes of its own: the browser has it
	// at that address, as it is. It lists both, and a stream that is
	// asked for one shows that one, while the first stream's Browser Tab
	// stays what it was.
	const linked = "http://127.0.0.1:8000/third.html?redirect_uri=http%3A%2F%2Flocalhost%3A8085%2Fcallback&scope=a%20b+c#state"
	if tab, err := viewport.NewTab(ctx, dial, browser.ViewportStream.Port, linked); err != nil || tab.ID == "" || tab.Address != linked {
		t.Fatalf("the Browser Tab opened at %s is %+v (%v)", linked, tab, err)
	}
	var third, second string
	waitFor(t, 10*time.Second, "the browser to list its second Browser Tab", func() bool {
		tabs, err := stream.Tabs(ctx)
		if err != nil || len(tabs) != 2 {
			return false
		}
		third, second = "", ""
		for _, tab := range tabs {
			switch tab.Title {
			case "third":
				third = tab.ID
			case "second":
				second = tab.ID
			}
		}
		return third != "" && second == stream.Tab()
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
	// other alone, whose streams go on.
	if err := other.CloseTab(ctx); err != nil {
		t.Fatalf("closing the second Browser Tab: %v", err)
	}
	waitFor(t, 10*time.Second, "the browser to list one Browser Tab again", func() bool {
		tabs, err := stream.Tabs(ctx)
		return err == nil && len(tabs) == 1 && tabs[0].ID == second
	})
	corner(back, "the first Browser Tab, once the second was closed", func(r, g, b uint32) bool { return r > g && r > b })
	if s, err := viewport.OpenCDP(ctx, dial, browser.ViewportStream.Port, "no-such-tab"); err == nil {
		s.Close()
		t.Error("a stream was opened on a Browser Tab the browser does not have")
	}
}
