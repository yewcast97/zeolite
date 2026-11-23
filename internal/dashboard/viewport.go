package dashboard

import (
	"context"
	"image"
	"time"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/pack"
	"zeolite/internal/viewport"
)

// daemonReady reports whether a pack's daemon accepts connections.
func (v *sandboxView) daemonReady(name string) bool {
	for _, d := range v.workstation.Daemons {
		if d.Pack == name {
			return d.Ready
		}
	}
	return false
}

func (v *sandboxView) viewportOf(m pack.Manifest) *viewportView {
	vp := v.viewports[m.Name]
	if vp == nil {
		vp = &viewportView{sv: v, pack: m, shown: make(chan struct{}, 1), listed: make(chan struct{}, 1)}
		v.viewports[m.Name] = vp
	}
	return vp
}

// tabsEvery is how long after a browser's Browser Tabs were listed, and
// the listing was built into a frame of the window, they are listed
// again; tabsPatience bounds one listing, and tabTitle is how many
// characters of a Browser Tab's title its tab shows.
var tabsEvery = time.Second

const (
	tabsPatience = 10 * time.Second
	tabTitle     = 28
)

// viewportView shows one Viewport Stream and sends it the pointer and
// the keyboard. Over a browser's it has the Address Bar, and over that
// the strip of the browser's Browser Tabs where it has more than one.
type viewportView struct {
	sv   *sandboxView
	pack pack.Manifest

	opening bool
	stream  ViewportStream
	cancel  context.CancelFunc
	err     string

	bitmap     *ui.Bitmap
	frameW     int
	frameH     int
	pageW      float64
	pageH      float64
	boxW, boxH float32
	// shown is signalled by each build of the view: the stream is asked
	// for its next frame only after the one before was built into a
	// frame of the window, so a viewport nobody sees asks for nothing.
	// listed is the same for the listing of the Browser Tabs.
	shown  chan struct{}
	listed chan struct{}

	// tabs are the browser's Browser Tabs, each where the viewport first
	// saw it; tab is the ID of the one shown, empty for the browser's
	// first; and chosen is the strip's chosen tab, which is another than
	// the one shown once its user chose it.
	tabs   []viewport.BrowserTab
	tab    string
	chosen int
	// address is what the Address Bar holds: the address of the Browser
	// Tab shown, or what is typed there while the bar has the keyboard.
	// bar is the bar's field, and frames the box that shows the frames,
	// which has the keyboard again once an address is sent.
	address string
	bar     ui.Handle
	frames  ui.Handle
	// sent is the address the Browser Tab shown was last sent to, and
	// sentFrom the address the browser listed for it then: until it
	// lists another, the tab is on its way, and the bar holds sent.
	sent, sentFrom string
}

func (vp *viewportView) close() {
	if vp.cancel != nil {
		vp.cancel()
	}
	if vp.stream != nil {
		vp.stream.Close()
	}
	vp.stream, vp.cancel, vp.opening = nil, nil, false
}

// open opens the stream, on the Browser Tab that is to be shown, and
// feeds its frames and the browser's Browser Tabs to the view.
func (vp *viewportView) open() {
	vp.opening, vp.err = true, ""
	d, id, tab := vp.sv.d, vp.sv.id, vp.tab
	ctx, cancel := context.WithCancel(context.Background())
	vp.cancel = cancel
	go func() {
		stream, err := d.backend.OpenViewport(id, vp.pack, tab)
		if err != nil {
			d.update(func() {
				// Closed since, it has nothing to say. Otherwise the
				// browser's first Browser Tab is the next to try.
				if ctx.Err() == nil {
					vp.opening, vp.err, vp.tab = false, err.Error(), ""
					vp.chosen = vp.shownIndex()
				}
			})
			return
		}
		d.update(func() {
			if ctx.Err() != nil {
				stream.Close()
				return
			}
			vp.stream, vp.tab = stream, stream.Tab()
			vp.chosen = vp.shownIndex()
		})
		go vp.listTabs(ctx, stream)
		for {
			frame, err := stream.Next(ctx)
			if err != nil {
				d.update(func() {
					if ctx.Err() != nil {
						return
					}
					vp.close()
					// A Browser Tab that was closed takes its stream with
					// it: where the browser has another, that one is
					// shown.
					for _, other := range vp.tabs {
						if other.ID != vp.tab {
							vp.tabs = []viewport.BrowserTab{other}
							vp.tab, vp.chosen = other.ID, 0
							return
						}
					}
					vp.err = "The stream ended: " + err.Error()
				})
				return
			}
			// Forget builds from before this frame, then wait for the
			// one that shows it.
			select {
			case <-vp.shown:
			default:
			}
			d.update(func() { vp.show(frame) })
			select {
			case <-vp.shown:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// listTabs lists the browser's Browser Tabs for the view until ctx
// ends: again a moment after each listing was built into a frame of the
// window, so that a viewport nobody sees asks for nothing.
func (vp *viewportView) listTabs(ctx context.Context, stream ViewportStream) {
	d := vp.sv.d
	for {
		listing, cancel := context.WithTimeout(ctx, tabsPatience)
		tabs, err := stream.Tabs(listing)
		cancel()
		if ctx.Err() != nil {
			return
		}
		// Forget builds from before this listing, then wait for the one
		// that shows it.
		select {
		case <-vp.listed:
		default:
		}
		d.update(func() {
			// A listing that failed leaves the one before: the stream
			// says so itself when the browser is gone.
			if err == nil && vp.stream == stream {
				vp.list(tabs)
			}
		})
		select {
		case <-vp.listed:
		case <-ctx.Done():
			return
		}
		select {
		case <-time.After(tabsEvery):
		case <-ctx.Done():
			return
		}
	}
}

// list takes the browser's Browser Tabs as it lists them now. Each
// keeps the place the viewport first saw it at, and a new one comes
// last: the browser itself lists the one last used first. A tab its
// user chose and the view has not acted on yet stays chosen.
func (vp *viewportView) list(tabs []viewport.BrowserTab) {
	wanted := ""
	if vp.chosen != vp.shownIndex() && vp.chosen >= 0 && vp.chosen < len(vp.tabs) {
		wanted = vp.tabs[vp.chosen].ID
	}
	now := make(map[string]viewport.BrowserTab, len(tabs))
	for _, tab := range tabs {
		now[tab.ID] = tab
	}
	var kept []viewport.BrowserTab
	for _, tab := range vp.tabs {
		if latest, there := now[tab.ID]; there {
			kept = append(kept, latest)
			delete(now, tab.ID)
		}
	}
	for _, tab := range tabs {
		if _, unseen := now[tab.ID]; unseen {
			kept = append(kept, tab)
		}
	}
	vp.tabs = kept
	vp.chosen = vp.shownIndex()
	for i, tab := range vp.tabs {
		if tab.ID == wanted {
			vp.chosen = i
		}
	}
}

// shownIndex is where the Browser Tab shown stands among the Browser
// Tabs: first, while it is not among them.
func (vp *viewportView) shownIndex() int {
	for i, tab := range vp.tabs {
		if tab.ID == vp.tab {
			return i
		}
	}
	return 0
}

// shownAddress is the address of the Browser Tab shown: where it was
// sent, while the browser still lists the address it was sent from, and
// otherwise what the browser last listed.
func (vp *viewportView) shownAddress() string {
	listed := ""
	for _, tab := range vp.tabs {
		if tab.ID == vp.tab {
			listed = tab.Address
		}
	}
	if vp.sent != "" && listed == vp.sentFrom {
		return vp.sent
	}
	vp.sent = ""
	return listed
}

// switchTo shows another Browser Tab: the stream of the one shown is
// closed, and the next build opens the other's. The frame it showed
// stays until the other's first.
func (vp *viewportView) switchTo(tab string) {
	vp.close()
	vp.tab, vp.sent = tab, ""
	vp.chosen = vp.shownIndex()
}

// navigate sends the Browser Tab shown to what the Address Bar holds,
// and gives the keyboard back to the frames. The bar holds the address
// it sent until the browser lists another than it was sent from.
func (vp *viewportView) navigate(c *ui.Context) {
	stream, address := vp.stream, viewport.Address(vp.address)
	if stream == nil || address == "" {
		return
	}
	vp.sent = ""
	vp.sentFrom = vp.shownAddress()
	vp.sent, vp.address = address, address
	vp.frames.Focus(c)
	go stream.Navigate(address)
}

// tabTitles are what the strip's tabs say: each Browser Tab's own
// title, or its address where it has none, cut to a tab's length.
func (vp *viewportView) tabTitles() []string {
	titles := make([]string, len(vp.tabs))
	for i, tab := range vp.tabs {
		title := tab.Title
		if title == "" {
			title = tab.Address
		}
		if title == "" {
			title = "New Browser Tab"
		}
		if runes := []rune(title); len(runes) > tabTitle {
			title = string(runes[:tabTitle]) + "…"
		}
		titles[i] = title
	}
	return titles
}

// show puts a frame in the bitmap. A frame as JPEG decodes it is given
// to the GPU as it is; anything else as RGBA.
func (vp *viewportView) show(f viewport.Frame) {
	b := f.Image.Bounds()
	if vp.bitmap == nil || b.Dx() != vp.frameW || b.Dy() != vp.frameH {
		vp.frameW, vp.frameH = b.Dx(), b.Dy()
		vp.bitmap = ui.NewBitmap(image.NewRGBA(image.Rect(0, 0, vp.frameW, vp.frameH)))
	}
	vp.pageW, vp.pageH = f.PageWidth, f.PageHeight
	if ycc, ok := f.Image.(*image.YCbCr); ok && ycc.SubsampleRatio == image.YCbCrSubsampleRatio420 {
		vp.bitmap.UpdateYUV(&ui.YUVImage{
			Format:    ui.I420,
			Width:     b.Dx(),
			Height:    b.Dy(),
			Planes:    [3][]byte{ycc.Y, ycc.Cb, ycc.Cr},
			Strides:   [3]int{ycc.YStride, ycc.CStride, ycc.CStride},
			Matrix:    ui.BT601,
			FullRange: true,
		})
		return
	}
	vp.bitmap.Update(f.Image)
}

// toPage maps a point of the view's box to the page the frame shows,
// which is fitted inside the box with its proportions kept.
func (vp *viewportView) toPage(x, y float32) (float64, float64) {
	if vp.boxW <= 0 || vp.boxH <= 0 || vp.frameW == 0 || vp.frameH == 0 {
		return 0, 0
	}
	scale := min(vp.boxW/float32(vp.frameW), vp.boxH/float32(vp.frameH))
	offX := (vp.boxW - float32(vp.frameW)*scale) / 2
	offY := (vp.boxH - float32(vp.frameH)*scale) / 2
	fx := float64((x-offX)/scale) / float64(vp.frameW)
	fy := float64((y-offY)/scale) / float64(vp.frameH)
	fx, fy = max(0, min(1, fx)), max(0, min(1, fy))
	return fx * vp.pageW, fy * vp.pageH
}

// viewportKeys are the keys that type no text, as the stream names them.
var viewportKeys = map[ui.Key]viewport.Key{
	ui.KeyEnter: viewport.KeyEnter, ui.KeyTab: viewport.KeyTab, ui.KeyBackspace: viewport.KeyBackspace,
	ui.KeyDelete: viewport.KeyDelete, ui.KeyEscape: viewport.KeyEscape,
	ui.KeyLeft: viewport.KeyLeft, ui.KeyRight: viewport.KeyRight, ui.KeyUp: viewport.KeyUp, ui.KeyDown: viewport.KeyDown,
	ui.KeyHome: viewport.KeyHome, ui.KeyEnd: viewport.KeyEnd, ui.KeyPageUp: viewport.KeyPageUp, ui.KeyPageDown: viewport.KeyPageDown,
}

func viewportMods(m ui.Modifiers) viewport.Modifiers {
	var out viewport.Modifiers
	if m&ui.Shift != 0 {
		out |= viewport.Shift
	}
	if m&ui.Ctrl != 0 {
		out |= viewport.Control
	}
	if m&ui.Alt != 0 {
		out |= viewport.Alt
	}
	if m&ui.Super != 0 {
		out |= viewport.Meta
	}
	return out
}

// handle forwards input to the stream. Sending is a write to a
// connection, which is left to a goroutine so that the main thread
// never waits on the sandbox.
func (vp *viewportView) handle(ev ui.InputEvent) bool {
	stream := vp.stream
	if stream == nil {
		return false
	}
	x, y := vp.toPage(ev.X, ev.Y)
	mods := viewportMods(ev.Mods)
	var send func()
	switch ev.Kind {
	case ui.InputPointerMove:
		send = func() { stream.Pointer(viewport.PointerMoved, x, y, ev.Button, 0, mods) }
	case ui.InputPointerDown:
		send = func() { stream.Pointer(viewport.PointerPressed, x, y, ev.Button, max(1, ev.Clicks), mods) }
	case ui.InputPointerUp:
		send = func() { stream.Pointer(viewport.PointerReleased, x, y, ev.Button, max(1, ev.Clicks), mods) }
	case ui.InputScroll:
		send = func() { stream.Scroll(x, y, float64(ev.DX), float64(ev.DY), mods) }
	case ui.InputText:
		send = func() { stream.Text(ev.Text) }
	case ui.InputKeyDown, ui.InputKeyUp:
		key, ok := viewportKeys[ev.Key]
		if !ok {
			return false
		}
		down := ev.Kind == ui.InputKeyDown
		send = func() { stream.PressKey(key, down, mods) }
	default:
		return false
	}
	go send()
	return true
}

func (vp *viewportView) view(c *ui.Context) {
	t := c.Theme()
	// This build shows whatever frame the bitmap holds, and whatever
	// Browser Tabs were last listed.
	for _, built := range []chan struct{}{vp.shown, vp.listed} {
		select {
		case built <- struct{}{}:
		default:
		}
	}
	if vp.err != "" {
		ui.Column(c).Grow(1).Center().Gap(t.Space(2)).Children(func() {
			ui.Text(c, vp.err).TextColor(t.Danger)
			if ui.Button(c, "Try Again").Key("viewport-retry").Clicked() {
				vp.err = ""
			}
		})
		return
	}
	// The strip's tab that its user chose is the Browser Tab to show.
	if vp.chosen != vp.shownIndex() && vp.chosen >= 0 && vp.chosen < len(vp.tabs) {
		vp.switchTo(vp.tabs[vp.chosen].ID)
	}
	waiting := vp.stream == nil && !vp.opening
	if waiting && vp.sv.daemonReady(vp.pack.Name) {
		vp.open()
	}
	switch {
	case vp.bitmap != nil:
	case waiting:
		vp.sv.note(c, "Waiting for "+vp.pack.Title+" to be ready…")
		return
	default:
		vp.sv.note(c, "Connecting to "+vp.pack.Title+"…")
		return
	}
	if len(vp.tabs) > 1 {
		tabStrip(c, "Browser Tabs of "+vp.pack.Title, &vp.chosen, vp.tabTitles())
	}
	// The Address Bar follows the Browser Tab shown, but while something
	// is typed there.
	if !vp.bar.Focused(c) {
		vp.address = vp.shownAddress()
	}
	ui.Row(c).Shrink(0).FillWidth().MinHeight(stripHeight).AlignItems(ui.Center).Padding(t.Space(1), t.Space(2)).Font(chromeFont).
		Background(panelTheme.Background).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
		c.Key("viewport-address-" + vp.pack.Name)
		ui.TextInput(c, &vp.address).Label("Address of " + vp.pack.Title).Placeholder("Address").FontSize(textSmall).Grow(1).
			Bind(&vp.bar).OnSubmit(func() { vp.navigate(c) })
	})
	ui.Box(c).Grow(1).FillWidth().Key("viewport").Label(vp.pack.Title + " viewport").Focusable().Bind(&vp.frames).
		Background(t.Background).
		Draw(func(_ *ui.Painter, r ui.Rect) { vp.boxW, vp.boxH = r.W, r.H }).
		HandleInput(vp.handle).
		Children(func() {
			ui.Image(c, vp.bitmap).Fit(ui.Contain).Fill()
		})
}
