package viewport

import (
	"context"
	"strings"
	"unicode/utf16"
)

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

// Text types text, as a keyboard or an input method commits it: in the
// place of what was composed, where something was.
func (s *Stream) Text(text string) error {
	return s.send("Input.insertText", map[string]any{"text": text})
}

// Compose shows text where the page takes text, as what an input method
// composes, with the caret after caret of its characters. An empty text
// ends the composition and leaves nothing of it.
func (s *Stream) Compose(text string, caret int) error {
	// The browser counts the caret in UTF-16 units.
	runes := []rune(text)
	at := len(utf16.Encode(runes[:max(0, min(caret, len(runes)))]))
	return s.send("Input.imeSetComposition", map[string]any{"text": text, "selectionStart": at, "selectionEnd": at})
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

// EditCommand is a command of an Edit menu, by the browser's name for
// it.
type EditCommand string

const (
	EditSelectAll EditCommand = "selectAll"
	EditCut       EditCommand = "cut"
	EditUndo      EditCommand = "undo"
	EditRedo      EditCommand = "redo"
)

// Edit has the page do an editing command where it has the keyboard. A
// cut leaves what it took in the browser's own clipboard, not this
// Mac's.
func (s *Stream) Edit(command EditCommand) error {
	// The command rides a key event that names no key.
	if err := s.send("Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "commands": []string{string(command)}}); err != nil {
		return err
	}
	return s.send("Input.dispatchKeyEvent", map[string]any{"type": "keyUp"})
}

// selection is what the page is asked for its selection with: the text
// selected in the field that has the keyboard, but in one for a
// password, or else in the document, of the innermost frame that has
// the keyboard and is the page's own to read.
const selection = `(() => {
	let w = window, d = document;
	for (let a = d.activeElement; a && (a.tagName === "IFRAME" || a.tagName === "FRAME"); a = d.activeElement) {
		try { if (!a.contentDocument) break; d = a.contentDocument; w = a.contentWindow } catch (e) { break }
	}
	const a = d.activeElement;
	if (a && (a.tagName === "TEXTAREA" || a.tagName === "INPUT") && typeof a.selectionStart === "number") {
		return a.type === "password" ? "" : a.value.substring(a.selectionStart, a.selectionEnd);
	}
	return String(w.getSelection());
})()`

// maxSelection is how many bytes of a selection are taken.
const maxSelection = 1 << 20

// Selection is the text selected in the Browser Tab the stream shows,
// which the page is asked for as long as ctx lasts: empty where nothing
// is selected, in a field for a password, and in a frame of another
// site, which the page cannot read. It is the page's word, and no more
// than maxSelection of it.
func (s *Stream) Selection(ctx context.Context) (string, error) {
	var answer struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := s.ask(ctx, "Runtime.evaluate", map[string]any{"expression": selection, "returnByValue": true}, &answer); err != nil {
		return "", err
	}
	text, _ := answer.Result.Value.(string)
	if len(text) > maxSelection {
		text = strings.ToValidUTF8(text[:maxSelection], "")
	}
	return text, nil
}
