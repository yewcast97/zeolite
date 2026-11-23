package dashboard

import (
	_ "embed"
	"sync"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"zeolite/internal/palette"
)

// The window's looks, all made of the one palette: the theme of its
// controls, the theme of its panels, and the colors of the Sandbox
// Terminal.

// The type scale: the sizes of text the window uses, and no others.
const (
	textSmall  = 11 // labels of facts, what a Pane says while it shows no session
	textBody   = 13 // everything else
	textTitle  = 15 // a sandbox's name
	textNotice = 16 // what an empty page says
)

// chromeFont is the family of the words around the terminals: names,
// tabs, the labels of a form's parts. It is the terminals' own, so that
// the window reads as one screen of characters.
const chromeFont = "monospace"

// The measures of the window's frame.
const (
	// headerHeight is the height of the Header, the bar at the top of a
	// page, under which the window's own title bar hides.
	headerHeight = 48
	// sidebarWidth is how wide the sidebar is until its user drags its
	// edge.
	sidebarWidth = 220
)

// The measures of glyphs.
const (
	// glyphCell is the width of a cell of a control's glyph and of a
	// state's, and stateCell that of the glyph of its state that a
	// sandbox shows large while it starts.
	glyphCell = 2
	stateCell = 6
	// cardGlyph is the side of the glyph a Shared Folder's row leads
	// with, and what the height of a row of the sidebar is measured by:
	// twelve cells of glyphCell.
	cardGlyph = 12 * glyphCell
	// glyphButtonSize is the side of a control that shows a glyph.
	glyphButtonSize = 26
	// sessionGlyph is the side of an Agent Session's glyph in the Header:
	// its agent's twelve cells of glyphCell, with room around them for
	// the Attention mark, whose cells are markCell wide.
	sessionGlyph = 32
	markCell     = 1
	// stripHeight is the height of the strip over a Pane, with its name
	// and its controls, and of the tabs over the viewports.
	stripHeight = 32
)

// The measures of the Setup's form.
const (
	// formWidth is the widest the form grows.
	formWidth = 660
	// fieldNarrow is the width of a control that holds a number or a
	// short word.
	fieldNarrow = 150
	// fieldChoice is the width of a choice among a few.
	fieldChoice = 230
)

// The measures of the Opening, in characters of its grid: a character is
// half as wide as its line is high, and as large as the window lets the
// whole grid be.
const (
	// openingColumns and openingRows are the grid's size.
	openingColumns = 130
	openingRows    = 64
	// openingUnit is how many lines one unit of the cage's own measure
	// takes, in which its T-atoms stand at 0, 1 and 2; a glyph, six lines
	// high, is a little more than half a unit.
	openingUnit = 11
	// openingCageRow is the line of the cage's middle, and openingNameRow
	// the line of the app's name under it.
	openingCageRow = 29
	openingNameRow = 61
)

// tipWidth is the widest a Card's facts are shown.
const tipWidth = 380

// windowTheme is the theme of the window: dark, soft, in the palette's
// colors.
var windowTheme = func() *ui.Theme {
	t := ui.DarkTheme()
	t.Background = ui.Hex(palette.Background)
	t.Surface = ui.Hex(palette.Raised)
	t.SurfaceHover = ui.Hex(palette.RaisedHover)
	t.SurfacePressed = ui.Hex(palette.RaisedPressed)
	t.Border = ui.Hex(palette.Border)
	t.Text = ui.Hex(palette.Text)
	t.TextMuted = ui.Hex(palette.Muted)
	t.Accent = ui.Hex(palette.Accent)
	t.AccentHover = ui.Hex(palette.AccentHover)
	t.AccentPressed = ui.Hex(palette.AccentPressed)
	t.AccentText = ui.Hex(palette.AccentText)
	// An inactive window keeps its colors: it still says what its gates do.
	t.InactiveAccent = t.Accent
	t.Danger = ui.Hex(palette.Danger)
	t.Warning = ui.Hex(palette.Warning)
	t.Success = ui.Hex(palette.Success)
	t.Selection = ui.Hex(palette.Selection)
	t.Focus = ui.Hex(palette.Accent).Alpha(0.6)
	t.Inverse, t.InverseText = ui.Hex(palette.Raised), ui.Hex(palette.Text)
	t.Scrollbar = ui.Hex(palette.Muted).Alpha(0.35)
	t.Radius = 6
	t.FontSize = textBody
	// What a glyph says is told soon after the pointer rests on it.
	t.TooltipDelay = 350 * time.Millisecond
	return t
}()

// onStatus is the color of text on a status color: on a banner.
var onStatus = ui.Hex(palette.OnStatus)

// attention is the color of the Attention mark.
var attention = ui.Hex(palette.Attention)

// panelTheme is the theme of what stands beside the Sandbox Terminal:
// the sidebar and the bars. It is the window's on the panel's color, and
// its chosen row is a quiet gray, not the accent: the accent is for what
// can be done, a chosen row is only where one is.
var panelTheme = func() *ui.Theme {
	t := *windowTheme
	t.Background = ui.Hex(palette.Panel)
	t.Accent = ui.Hex(palette.RaisedHover)
	t.AccentHover = ui.Hex(palette.RaisedPressed)
	t.AccentPressed = ui.Hex(palette.RaisedPressed)
	t.AccentText = ui.Hex(palette.Text)
	t.InactiveAccent = ui.Hex(palette.Raised)
	return &t
}()

// symbolFont is the font of the symbols agents draw their screens with
// and no font of the system has: the icons of the Nerd Fonts project,
// which terminals ship for the same reason. Its license stands beside it.
//
//go:embed symbolfont/SymbolsNerdFontMono-Regular.ttf
var symbolFont []byte

// symbolFamily is the family symbolFont is registered under.
const symbolFamily = "Symbols Nerd Font Mono"

// terminalFont is the font of a Terminal Session: Menlo, which every Mac
// has, and the symbols' font for what Menlo lacks. The system's own
// monospace is not first: it answers for the symbols' code points with
// a box of its own, and the symbols' font is then never asked.
var terminalFont = terminal.Font{Family: "Menlo, " + symbolFamily + ", monospace"}

// registerSymbolFont makes the symbols' font one the window can draw
// with, once.
var registerSymbolFont = sync.OnceValue(func() error { return ui.RegisterFont(symbolFont, symbolFamily) })

// terminalTheme colors a Terminal Session.
var terminalTheme = func() *terminal.Theme {
	t := &terminal.Theme{
		Foreground: ui.Hex(palette.Text),
		Background: ui.Hex(palette.Background),
		Cursor:     ui.Hex(palette.Accent),
		CursorText: ui.Hex(palette.Background),
		Selection:  ui.Hex(palette.Selection),
	}
	for i, color := range palette.Terminal {
		t.Palette[i] = ui.Hex(color)
	}
	return t
}()
