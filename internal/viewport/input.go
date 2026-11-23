package viewport

// Input into a stream. Positions are in the page's units, as
// Frame.PageWidth and PageHeight measure it.

// Modifiers are the modifier keys held, as the DevTools Protocol counts
// them.
type Modifiers int

const (
	Alt Modifiers = 1 << iota
	Control
	Meta
	Shift
)

// PointerEvent is what a pointer did.
type PointerEvent string

const (
	PointerMoved    PointerEvent = "mouseMoved"
	PointerPressed  PointerEvent = "mousePressed"
	PointerReleased PointerEvent = "mouseReleased"
)

// buttons names the pointer's buttons by number: primary, secondary, middle.
var buttons = []string{"left", "right", "middle"}

// Pointer sends a pointer event at a position. button is 0 for the
// primary button, 1 the secondary and 2 the middle, or -1 for none;
// clicks counts quick successive presses.
func (s *Stream) Pointer(event PointerEvent, x, y float64, button, clicks int, mods Modifiers) error {
	name := "none"
	if button >= 0 && button < len(buttons) {
		name = buttons[button]
	}
	return s.send("Input.dispatchMouseEvent", map[string]any{
		"type":       string(event),
		"x":          x,
		"y":          y,
		"button":     name,
		"clickCount": clicks,
		"modifiers":  int(mods),
	})
}

// Scroll scrolls by dx, dy at a position.
func (s *Stream) Scroll(x, y, dx, dy float64, mods Modifiers) error {
	return s.send("Input.dispatchMouseEvent", map[string]any{
		"type":      "mouseWheel",
		"x":         x,
		"y":         y,
		"deltaX":    dx,
		"deltaY":    dy,
		"modifiers": int(mods),
	})
}

// Text types text, as a keyboard or an input method commits it.
func (s *Stream) Text(text string) error {
	return s.send("Input.insertText", map[string]any{"text": text})
}

// Key is a key that types no text, by its name in the UI Events
// specification.
type Key string

const (
	KeyEnter     Key = "Enter"
	KeyTab       Key = "Tab"
	KeyBackspace Key = "Backspace"
	KeyDelete    Key = "Delete"
	KeyEscape    Key = "Escape"
	KeyLeft      Key = "ArrowLeft"
	KeyRight     Key = "ArrowRight"
	KeyUp        Key = "ArrowUp"
	KeyDown      Key = "ArrowDown"
	KeyHome      Key = "Home"
	KeyEnd       Key = "End"
	KeyPageUp    Key = "PageUp"
	KeyPageDown  Key = "PageDown"
)

// virtualKeyCodes are the Windows virtual key codes of the keys, which
// the browser needs to act on a key and not only report it to the page.
var virtualKeyCodes = map[Key]int{
	KeyEnter: 13, KeyTab: 9, KeyBackspace: 8, KeyDelete: 46, KeyEscape: 27,
	KeyLeft: 37, KeyUp: 38, KeyRight: 39, KeyDown: 40,
	KeyHome: 36, KeyEnd: 35, KeyPageUp: 33, KeyPageDown: 34,
}

// PressKey presses or releases a key.
func (s *Stream) PressKey(key Key, down bool, mods Modifiers) error {
	event := "keyUp"
	if down {
		event = "rawKeyDown"
	}
	params := map[string]any{
		"type":                  event,
		"key":                   string(key),
		"code":                  string(key),
		"windowsVirtualKeyCode": virtualKeyCodes[key],
		"modifiers":             int(mods),
	}
	if err := s.send("Input.dispatchKeyEvent", params); err != nil {
		return err
	}
	// Enter and Tab also type a character, which is what a form acts on.
	if text := map[Key]string{KeyEnter: "\r", KeyTab: "\t"}[key]; down && text != "" {
		return s.send("Input.dispatchKeyEvent", map[string]any{"type": "char", "text": text, "modifiers": int(mods)})
	}
	return nil
}
