// Package palette holds the app's colors, as hexadecimal strings: a soft
// dark theme modeled on Zed's One Dark. It imports nothing, so that the
// dashboard, the window and the icon's generator all read one palette,
// each making of a color what its own drawing needs.
package palette

// The window, from back to front.
const (
	// Background is behind the Sandbox Terminal and the pages.
	Background = "#282c33"
	// Panel is behind what stands beside them: the sidebar, the bars.
	Panel = "#2f343e"
	// Raised is the face of controls on a panel, and RaisedHover and
	// RaisedPressed while the pointer is over them or presses them.
	Raised        = "#3b414d"
	RaisedHover   = "#454b58"
	RaisedPressed = "#4e5564"
	// Border separates panels and outlines controls.
	Border = "#464b57"
	// Text is the color of text, and Muted that of secondary text.
	Text  = "#dce0e5"
	Muted = "#a9afbc"
)

// Colors that mean something.
const (
	// Accent marks the primary action and the keyboard focus, and
	// AccentText is text on it.
	Accent        = "#74ade8"
	AccentHover   = "#8dbcee"
	AccentPressed = "#5f9bd9"
	AccentText    = "#1b1f26"
	// Success, Warning and Danger are an open gate, a sandbox on its way,
	// and a locked gate or an error: the colors of a state's glyph and
	// the fills of banners. OnStatus is text on any of them.
	Success  = "#a1c181"
	Warning  = "#dec184"
	Danger   = "#d97c81"
	OnStatus = "#1b1f26"
	// Attention is the Attention mark: what needs the user, where it is
	// not in sight. It is read on a panel and on the background, and
	// OnStatus is text on it.
	Attention = "#e0a16b"
	// Selection highlights selected text, over Background.
	Selection = "#3d5a80"
)

// Terminal is the sixteen colors programs in a Sandbox Terminal draw
// with: black, red, green, yellow, blue, magenta, cyan and white, then
// their bright kinds.
var Terminal = [16]string{
	"#3b414d", "#d97c81", "#a1c181", "#dec184", "#74ade8", "#bd84d5", "#6eb4bf", "#dce0e5",
	"#6f7684", "#e0898d", "#b5d19a", "#e9d29f", "#93c1f0", "#c794dd", "#8ac7d1", "#fafbfc",
}
