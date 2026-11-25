package dashboard

import (
	"context"
	"image"
	"strings"
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

// tabsEvery is how long after a browser's Browser Tabs were listed, and
// the listing was built into a frame of the window, they are listed
// again; browserPatience bounds one question to the browser, a listing
// or where Back and Forward lead; tabTitle is how many characters of a
// Browser Tab's title its tab shows; and inputBacklog is how much input
// waits for a stream that takes none before more is dropped.
var tabsEvery = time.Second

const (
	browserPatience = 10 * time.Second
	tabTitle        = 28
	inputBacklog    = 256
)

// viewportView is a Viewport: what a Pane shows in the place of a
// Terminal Session. It shows one Viewport Stream and sends it the
// pointer and the keyboard. Over a browser's it has the Address Bar,
// with Back and Forward beside it, and over that the strip of the
// browser's Browser Tabs where it has more than one.
type viewportView struct {
	sv *sandboxView
	// b is the browser whose Browser Tab the Viewport shows, which lists
	// its Browser Tabs for every Viewport of it, and pack the browser's.
	b    *browser
	pack pack.Manifest

	opening bool
	stream  ViewportStream
	cancel  context.CancelFunc
	err     string
	// inputs is what the stream is still to be sent, in the order it
	// came: one goroutine sends it, so that the main thread never waits
	// on the sandbox and no input overtakes another.
	inputs chan func()

	bitmap     *ui.Bitmap
	frameW     int
	frameH     int
	pageW      float64
	pageH      float64
	boxW, boxH float32
	// shown is signalled by each build of the view: the stream is asked
	// for its next frame only after the one before was built into a
	// frame of the window, so a Viewport nobody sees asks for nothing.
	shown chan struct{}

	// tab is the ID of the Browser Tab shown, empty while there is none
	// yet; and chosen is the strip's chosen tab, which is another than
	// the one shown once its user chose it. choosing is whether the
	// Viewport waits for the browser to be listed, to take a Browser Tab
	// that no other Viewport shows: a listing later than since, the
	// number of them when it began to wait.
	tab      string
	chosen   int
	choosing bool
	since    int
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
	// toBar is whether the Address Bar is to have the keyboard at the
	// next build: a new Browser Tab's.
	toBar bool
	// closing is the stream whose Browser Tab the browser is being asked
	// to close.
	closing ViewportStream
	// caretX and caretY are where the pointer was last pressed on the
	// frames, in their box: what an input method composes shows there,
	// the page's own caret being the page's to know.
	caretX, caretY float32
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

// live is whether a Pane still shows the Viewport: not once its Pane
// closed, took a session, or went with its sandbox.
func (vp *viewportView) live() bool { return vp.sv.paneOfViewport(vp) != nil }

// choose has the Viewport take a Browser Tab once the browser is listed
// anew: the first that no other Viewport shows, or a new one.
func (vp *viewportView) choose() {
	vp.close()
	vp.tab, vp.choosing, vp.since = "", true, vp.b.listings
	vp.b.hurry()
}

// pick takes the first of the browser's Browser Tabs that no other
// Viewport shows. Where every one is shown, a blank one is opened for
// it, and the keyboard goes to the Address Bar, for where it is to go.
func (vp *viewportView) pick() {
	vp.choosing = false
	if tab := vp.b.free(""); tab != "" {
		vp.tab = tab
		return
	}
	vp.openTab(blankAddress, true, nil)
}

// open opens the stream, on the Browser Tab that is to be shown, and
// feeds its frames to the view.
func (vp *viewportView) open() {
	vp.opening, vp.err = true, ""
	d, id, tab := vp.sv.d, vp.sv.id, vp.tab
	ctx, cancel := context.WithCancel(context.Background())
	vp.cancel = cancel
	inputs := make(chan func(), inputBacklog)
	vp.inputs = inputs
	go func() {
		stream, err := d.backend.OpenViewport(id, vp.pack, tab)
		if err != nil {
			d.update(func() {
				// Closed since, it has nothing to say. Otherwise another
				// Browser Tab is the next to try.
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
		go func() {
			for {
				select {
				case input := <-inputs:
					input()
				case <-ctx.Done():
					return
				}
			}
		}()
		for {
			frame, err := stream.Next(ctx)
			if err != nil {
				d.update(func() {
					if ctx.Err() != nil {
						return
					}
					vp.close()
					// A Browser Tab that was closed takes its stream with
					// it: where the browser has another that no Viewport
					// shows, that one is shown.
					gone := vp.tab
					vp.tab = ""
					if other := vp.b.free(gone); other != "" {
						vp.tab = other
						vp.chosen = vp.shownIndex()
						return
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

// shownIndex is where the Browser Tab shown stands among the Browser
// Tabs: first, while it is not among them.
func (vp *viewportView) shownIndex() int {
	for i, tab := range vp.b.tabs {
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
	for _, tab := range vp.b.tabs {
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

// openLink takes a link that the sandbox's user Command-clicked in the
// session's Pane, and always reports that it did: nothing on this Mac
// opens a link a session printed. A web address, where the sandbox has
// a browser, opens in a new Browser Tab of that browser: in the first
// Pane that shows the browser, or in a Pane that is added for it beside
// the one that was clicked in, or below it, where a Viewport has room
// there. So an agent's link to sign in is opened where its answer is
// waited for. Anything else that was clicked, and a web address that no
// Pane can show, goes to this Mac's clipboard and nowhere else. The
// address is the session's word, the one thing a click takes of what a
// session printed.
func (s *session) openLink(link string) bool {
	v := s.sv
	from := v.paneOf(s)
	web := strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://")
	if b := v.linkBrowser(); web && b != nil && from != nil {
		if shown := b.views(); len(shown) > 0 {
			shown[0].openTab(link, false, nil)
			return true
		}
		if added := v.divideFor(from); added != nil {
			v.newViewport(added, b.pack).openTab(link, false, nil)
			return true
		}
	}
	v.copyLink(from, link)
	return true
}

// divideFor adds a Pane for a Viewport beside a Pane, or below it,
// where the half it would have is as large as a Viewport takes: nil
// where neither is, or the Pane cannot be divided.
func (v *sandboxView) divideFor(p *pane) *pane {
	half := func(length float32) float32 { return (length - 1) / 2 }
	if half(p.size.W) >= viewportMinWidth && p.size.H >= viewportMinHeight {
		if added := v.divide(p, false); added != nil {
			return added
		}
	}
	if p.size.W >= viewportMinWidth && half(p.size.H) >= viewportMinHeight {
		return v.divide(p, true)
	}
	return nil
}

// blankAddress is where a new Browser Tab is that was sent nowhere yet.
const blankAddress = "about:blank"

// openTab opens a new Browser Tab at an address, off the main thread,
// and shows it. One its user asked for is blank, and the keyboard goes
// to the Address Bar, which is emptied for where the tab is to go. then,
// where there is one, is told whether the new Browser Tab is the one
// shown now.
func (vp *viewportView) openTab(address string, typing bool, then func(shown bool)) {
	sv := vp.sv
	d, id := sv.d, sv.id
	go func() {
		tab, err := d.backend.OpenBrowserTab(id, vp.pack, address)
		d.update(func() {
			// The sandbox stopped meanwhile, or its Pane shows something
			// else by now: the Viewport is gone.
			if !vp.live() {
				return
			}
			if err != nil {
				vp.err = "The Browser Tab was not opened: " + err.Error()
			} else {
				vp.err = ""
				vp.switchTo(tab)
				// The browser lists it from now on.
				vp.b.hurry()
				if typing {
					vp.toBar, vp.address = true, ""
				}
			}
			if then != nil {
				then(err == nil)
			}
		})
	}()
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
	vp.send(func() { stream.Navigate(address) })
}

// send has the stream sent an input after those that came before it,
// and reports whether it will be. What comes while inputBacklog of them
// wait is dropped: the sandbox takes none then.
func (vp *viewportView) send(input func()) bool {
	select {
	case vp.inputs <- input:
		return true
	default:
		return false
	}
}

// along sends the Browser Tab shown along its own history, by Back or
// by Forward of its stream. The Address Bar holds what the browser
// lists from then on, not an address the tab was sent to before.
func (vp *viewportView) along(step func(ViewportStream, context.Context) error) {
	stream := vp.stream
	if stream == nil {
		return
	}
	vp.sent = ""
	vp.send(func() { asked(func(ctx context.Context) { step(stream, ctx) }) })
}

// backLabel, forwardLabel, newTabLabel and closeTabLabel are what the
// controls beside the Address Bar say they do.
const (
	backLabel     = "Back"
	forwardLabel  = "Forward"
	newTabLabel   = "New Browser Tab"
	closeTabLabel = "Close Browser Tab"
)

// asked runs a question to the browser for no longer than
// browserPatience.
func asked(question func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.Background(), browserPatience)
	defer cancel()
	question(ctx)
}

// copy puts the text selected in the page on this Mac's clipboard, as
// its user asked by Copy or Cut, and with Cut has the page cut it. It
// is the page's word, and goes nowhere else. Where the page says that
// nothing is selected, the clipboard stays as it is.
func (vp *viewportView) copy(cut bool) {
	stream, d := vp.stream, vp.sv.d
	services := d.services
	vp.send(func() {
		asked(func(ctx context.Context) {
			text, err := stream.Selection(ctx)
			if err != nil || text == "" {
				return
			}
			d.update(func() { services.WriteClipboard(text) })
			if cut {
				stream.Edit(viewport.EditCut)
			}
		})
	})
}

// closeTab closes the Browser Tab shown. Its stream ends with it, and
// the view shows another, that no other Viewport shows. Where there is
// none, as when it is the browser's last, it gives way to a blank one,
// which is opened and shown first, as New Browser Tab's is: a Viewport
// is never left with none, which shows nothing. A Browser Tab is asked
// to close once, however often its user asks.
func (vp *viewportView) closeTab() {
	stream := vp.stream
	if stream == nil || stream == vp.closing {
		return
	}
	vp.closing = stream
	if vp.b.free(vp.tab) != "" {
		if !vp.send(func() { vp.closeTabOf(stream) }) {
			vp.closing = nil
		}
		return
	}
	vp.openTab(blankAddress, true, func(shown bool) {
		if !shown {
			vp.closing = nil
			return
		}
		// Its stream is closed by now, and takes no input: the browser
		// is asked on a connection of its own.
		go vp.closeTabOf(stream)
	})
}

// closeTabOf has the browser close the Browser Tab a stream shows, and
// says so where it did not.
func (vp *viewportView) closeTabOf(stream ViewportStream) {
	asked(func(ctx context.Context) {
		err := stream.CloseTab(ctx)
		vp.sv.d.update(func() {
			if vp.closing != stream {
				return
			}
			vp.closing = nil
			if err != nil {
				vp.err = "The Browser Tab was not closed: " + err.Error()
			}
		})
	})
}

// tabTitles are what the strip's tabs say: each Browser Tab's own
// title, or its address where it has none, cut to a tab's length.
func (vp *viewportView) tabTitles() []string {
	titles := make([]string, len(vp.b.tabs))
	for i, tab := range vp.b.tabs {
		title := tab.Title
		if title == "" {
			title = tab.Address
		}
		if title == "" {
			title = newTabLabel
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

// editCommands are the commands of the Edit menu that the page does by
// itself, as the stream names them. Copy, Cut and Paste are the view's,
// by this Mac's clipboard.
var editCommands = map[string]viewport.EditCommand{
	"selectAll": viewport.EditSelectAll, "undo": viewport.EditUndo, "redo": viewport.EditRedo,
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

// handle forwards input to the stream, in the order it comes. A key
// that goes back or forward in a history does so in the Browser Tab's:
// a mouse's side buttons and a swipe between pages, which come as keys,
// and Command with a bracket. The Edit menu's commands are the page's:
// Copy and Cut put what is selected there on this Mac's clipboard, and
// Paste types what the clipboard holds.
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
		vp.caretX, vp.caretY = ev.X, ev.Y
		// A press on the frames of a Browser Tab that stands behind
		// another brings it forward, and is no press in its page.
		if stream.Behind() {
			vp.send(func() { stream.Front() })
			return true
		}
		send = func() { stream.Pointer(viewport.PointerPressed, x, y, ev.Button, max(1, ev.Clicks), mods) }
	case ui.InputPointerUp:
		send = func() { stream.Pointer(viewport.PointerReleased, x, y, ev.Button, max(1, ev.Clicks), mods) }
	case ui.InputScroll:
		send = func() { stream.Scroll(x, y, float64(ev.DX), float64(ev.DY), mods) }
	case ui.InputText:
		send = func() { stream.Text(ev.Text) }
	case ui.InputCompose:
		send = func() { stream.Compose(ev.Text, ev.Caret) }
	case ui.InputCommand:
		switch command, edits := editCommands[ev.Text]; {
		case edits:
			send = func() { stream.Edit(command) }
		case ev.Text == "copy" || ev.Text == "cut":
			vp.copy(ev.Text == "cut")
			return true
		case ev.Text == "paste":
			text := vp.sv.d.services.ReadClipboard()
			if text == "" {
				return true
			}
			send = func() { stream.Text(text) }
		default:
			return false
		}
	case ui.InputKeyDown, ui.InputKeyUp:
		down := ev.Kind == ui.InputKeyDown
		back := ev.Key == ui.KeyBack || ev.Mods == ui.Super && ev.Key == ui.KeyBracketLeft
		forward := ev.Key == ui.KeyForward || ev.Mods == ui.Super && ev.Key == ui.KeyBracketRight
		if back || forward {
			step := ViewportStream.Back
			if forward {
				step = ViewportStream.Forward
			}
			if down {
				vp.along(step)
			}
			return true
		}
		key, ok := viewportKeys[ev.Key]
		if !ok {
			return false
		}
		send = func() { stream.PressKey(key, down, mods) }
	default:
		return false
	}
	vp.send(send)
	return true
}

// behindSays is what a Viewport says while its Browser Tab stands
// behind another in the browser's window, where it draws nothing.
const behindSays = "Behind another Browser Tab: click to bring it forward"

// view is the Viewport of the Pane called name, under the Pane's strip.
func (vp *viewportView) view(c *ui.Context, name string) {
	t := c.Theme()
	// This build shows whatever frame the bitmap holds, and whatever
	// Browser Tabs the browser last listed.
	select {
	case vp.shown <- struct{}{}:
	default:
	}
	vp.b.seen()
	if vp.err != "" {
		ui.Column(c).Grow(1).FillWidth().Center().Gap(t.Space(2)).Padding(t.Space(3)).Children(func() {
			ui.Text(c, vp.err).TextColor(t.Danger)
			if ui.Button(c, "Try Again").Key("viewport-retry").Clicked() {
				vp.err = ""
				// A Viewport that was left with no Browser Tab takes
				// another.
				if vp.tab == "" {
					vp.choose()
				}
			}
		})
		return
	}
	// The strip's tab that its user chose is the Browser Tab to show. One
	// that another Pane shows changes places with this one's: no two
	// Panes show one Browser Tab.
	if tabs := vp.b.tabs; vp.chosen != vp.shownIndex() && vp.chosen >= 0 && vp.chosen < len(tabs) {
		want := tabs[vp.chosen].ID
		if other := vp.b.shownBy(want, vp); other != nil {
			vp.chosen = vp.shownIndex()
			sv := vp.sv
			sv.d.update(func() { sv.changePlaces(vp, other) })
		} else {
			vp.switchTo(want)
		}
	}
	ready := vp.sv.daemonReady(vp.pack.Name)
	if vp.stream == nil && !vp.opening && ready && vp.tab != "" {
		vp.open()
	}
	switch {
	case vp.bitmap != nil:
	case !ready:
		vp.sv.note(c, "Waiting for "+vp.pack.Title+" to be ready…")
		return
	default:
		vp.sv.note(c, "Connecting to "+vp.pack.Title+"…")
		return
	}
	if len(vp.b.tabs) > 1 {
		tabStrip(c, "Browser Tabs of "+name, &vp.chosen, vp.tabTitles())
	}
	// The Address Bar follows the Browser Tab shown, but while something
	// is typed there, and while it waits for the keyboard, emptied for
	// where a new Browser Tab is to go.
	if !vp.bar.Focused(c) && !vp.toBar {
		vp.address = vp.shownAddress()
	}
	in := func(label string) string { return label + " in " + name }
	// The row is the same one whether or not a strip stands over it: the
	// Address Bar keeps the keyboard when the browser's second Browser
	// Tab comes.
	ui.Row(c).Key("viewport-bar").Shrink(0).FillWidth().MinHeight(stripHeight).AlignItems(ui.Center).Gap(t.Space(1)).Padding(t.Space(1), t.Space(2)).Font(chromeFont).
		Background(panelTheme.Background).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
		if glyphButtonIn(c, "viewport-back", glyphBack, in(backLabel), backLabel, t.Text).Clicked() {
			vp.along(ViewportStream.Back)
		}
		if glyphButtonIn(c, "viewport-forward", glyphForward, in(forwardLabel), forwardLabel, t.Text).Clicked() {
			vp.along(ViewportStream.Forward)
		}
		c.Key("viewport-address")
		ui.TextInput(c, &vp.address).Label("Address of " + name).Placeholder("Address").FontSize(textSmall).Grow(1).Shrink(1).MinWidth(0).
			Bind(&vp.bar).OnSubmit(func() { vp.navigate(c) })
		if vp.toBar {
			vp.toBar = false
			vp.bar.Focus(c)
		}
		if glyphButtonIn(c, "viewport-new-tab", glyphAdd, in(newTabLabel), newTabLabel, t.Text).Clicked() {
			vp.openTab(blankAddress, true, nil)
		}
		if glyphButtonIn(c, "viewport-close-tab", glyphClose, in(closeTabLabel), closeTabLabel, t.Text).Clicked() {
			vp.closeTab()
		}
	})
	// A Browser Tab that stands behind another draws nothing: the frame
	// it last drew stays, and a line says why it stands still.
	behind := vp.stream != nil && vp.stream.Behind()
	if behind {
		ui.Row(c).Key("viewport-behind").Shrink(0).FillWidth().Padding(t.Space(1), t.Space(2)).Font(chromeFont).Background(panelTheme.Background).
			BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
			ui.Text(c, behindSays).FontSize(textSmall).TextColor(t.Warning).MaxLines(1)
		})
	}
	// The frames take text: the window gives what is typed, and what an
	// input method composes, only to what says that it does.
	ui.Box(c).Grow(1).FillWidth().Key("viewport").Label("Viewport of " + name).Focusable().Bind(&vp.frames).
		Background(t.Background).
		Draw(func(_ *ui.Painter, r ui.Rect) { vp.boxW, vp.boxH = r.W, r.H }).
		HandleInput(vp.handle).
		TextCaret(ui.Rect{X: vp.caretX, Y: vp.caretY, W: 1, H: textBody}).
		Children(func() {
			ui.Image(c, vp.bitmap).Fit(ui.Contain).Fill()
		})
}
