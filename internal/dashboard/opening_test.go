package dashboard

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/sandbox"
)

// text is the grid as lines of characters, a space where nothing is
// drawn.
func (g *openingGrid) text() string {
	var b strings.Builder
	for row := 0; row < openingRows; row++ {
		for col := 0; col < openingColumns; col++ {
			if c := g.cells[row*openingColumns+col]; c.char != 0 {
				b.WriteByte(c.char)
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// marksOf counts the characters of the grid that are marks of each
// agent's glyph, by the agent's number.
func (g *openingGrid) marksOf() map[int]int {
	marks := map[int]int{}
	for _, c := range g.cells {
		if c.agent != 0 {
			marks[int(c.agent)-1]++
		}
	}
	return marks
}

// The cage is a sodalite cage: twenty-four T-atoms, each at the same
// distance from the middle and bridged to three others, by thirty-six
// bridges in all.
func TestTheCageIsASodaliteCage(t *testing.T) {
	if len(cageAtoms) != 24 || len(cageBridges) != 36 {
		t.Fatalf("the cage has %d T-atoms and %d bridges, want 24 and 36", len(cageAtoms), len(cageBridges))
	}
	bridged := map[int]int{}
	for _, b := range cageBridges {
		bridged[b[0]]++
		bridged[b[1]]++
	}
	for i, a := range cageAtoms {
		if r := math.Sqrt(a[0]*a[0] + a[1]*a[1] + a[2]*a[2]); math.Abs(r-cageRadius) > 1e-9 {
			t.Errorf("T-atom %d is %.3f from the middle, want %.3f", i, r, cageRadius)
		}
		if bridged[i] != 3 {
			t.Errorf("T-atom %d has %d bridges, want 3", i, bridged[i])
		}
	}
}

// Through the Opening the whole cage is on the grid, and every agent the
// app knows has its glyph there, inside the cage: none is left out, none
// leaves, and each is seen nearly whole at some moment, whatever is in
// front of it at others.
func TestTheOpeningShowsEveryKnownAgentInTheCage(t *testing.T) {
	var g openingGrid
	most := map[int]int{}
	for at := openingDissolves; at <= openingLasts-openingDissolves; at += 50 * time.Millisecond {
		g.draw(at)
		text := g.text()
		for _, char := range []byte{atomNear, atomFar, bridgeMarks[0], bridgeMarks[len(bridgeMarks)-1]} {
			if !strings.ContainsRune(text, rune(char)) {
				t.Fatalf("after %v the grid holds no %q of the cage", at, char)
			}
		}
		if !strings.Contains(text, openingName) {
			t.Fatalf("after %v the grid does not hold the app's name", at)
		}
		// The cage is whole: no T-atom is off the grid.
		for i, atom := range cageAtoms {
			if col, row := place(turned(atom, at)); col < 1 || col > openingColumns-2 || row < 0 || row >= openingNameRow-1 {
				t.Fatalf("after %v T-atom %d is at column %.0f, line %.0f: off the grid, or on the name", at, i, col, row)
			}
		}
		marks := g.marksOf()
		for n, agent := range sandbox.KnownAgents {
			p := glyphAt(n, at)
			if far := math.Sqrt(p[0]*p[0] + p[1]*p[1] + p[2]*p[2]); far >= cageRoom {
				t.Fatalf("after %v the glyph of %s is %.2f from the middle: outside the cage, whose faces are %.2f from it", at, agent.Title, far, cageRoom)
			}
			if marks[n] == 0 {
				t.Errorf("after %v no mark of the glyph of %s is on the grid", at, agent.Title)
			}
			most[n] = max(most[n], marks[n])
		}
	}
	for n, agent := range sandbox.KnownAgents {
		whole := 0
		for _, line := range glyphLines(agentGlyphs[agent.Executable]) {
			for _, char := range line {
				if char != 0 {
					whole++
				}
			}
		}
		if whole == 0 {
			t.Fatalf("%s has no glyph", agent.Title)
		}
		if most[n]*10 < whole*9 {
			t.Errorf("the glyph of %s is never seen nearly whole: %d of its %d characters at most", agent.Title, most[n], whole)
		}
	}
}

// The Opening moves: the cage turns, every glyph wanders and bends as it
// goes, and a moment is the same however often it is drawn.
func TestTheOpeningMoves(t *testing.T) {
	var g, again openingGrid
	const at, later = 1500 * time.Millisecond, 1700 * time.Millisecond
	g.draw(at)
	before := g.text()
	again.draw(at)
	if again.text() != before {
		t.Error("one moment of the Opening was drawn two ways")
	}
	g.draw(later)
	if g.text() == before {
		t.Error("the Opening is the same a fifth of a second later")
	}
	if turned(cageAtoms[0], at) == turned(cageAtoms[0], later) {
		t.Error("the cage did not turn")
	}
	for n, agent := range sandbox.KnownAgents {
		if glyphAt(n, at) == glyphAt(n, later) {
			t.Errorf("the glyph of %s did not move", agent.Title)
		}
		bent := map[int]bool{}
		for line := 0; line < 6; line++ {
			for moment := time.Duration(0); moment < time.Second; moment += 50 * time.Millisecond {
				bent[glyphBends(n, line, moment)] = true
			}
		}
		if len(bent) < 3 {
			t.Errorf("the glyph of %s bends %d ways in a second, want it to bend to both sides", agent.Title, len(bent))
		}
		for m := n + 1; m < len(sandbox.KnownAgents); m++ {
			if glyphAt(n, at) == glyphAt(m, at) {
				t.Errorf("the glyphs of %s and %s are in one place", agent.Title, sandbox.KnownAgents[m].Title)
			}
		}
	}
	// It begins and ends on an empty grid, its characters coming and
	// going one by one.
	for _, edge := range []time.Duration{0, openingLasts} {
		g.draw(edge)
		if text := strings.TrimSpace(g.text()); text != "" {
			t.Errorf("after %v the grid holds %d characters, want none", edge, len(text))
		}
	}
	g.draw(openingDissolves / 2)
	half := len(strings.Join(strings.Fields(g.text()), ""))
	g.draw(openingDissolves)
	if all := len(strings.Join(strings.Fields(g.text()), "")); half == 0 || half >= all {
		t.Errorf("halfway through its coming the grid holds %d characters, and %d once come", half, all)
	}
}

// painted counts the pixels of the window's picture that are not the
// color of its first one, which is the background's.
func (h *harness) painted() int {
	h.t.Helper()
	img := h.tt.Image()
	behind := img.RGBAAt(img.Bounds().Min.X, img.Bounds().Min.Y)
	n := 0
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			if img.RGBAAt(x, y) != behind {
				n++
			}
		}
	}
	return n
}

// openingHarness is a dashboard of one sandbox that shows the Opening,
// in a window of the app's first size.
func openingHarness(t *testing.T) *harness {
	h := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	h.tt.SetSize(1280, 860)
	h.d.ShowOpening()
	return h
}

// The window shows the Opening and nothing of its page while the
// Opening lasts, draws it anew as time passes, and then shows the page.
func TestTheOpeningGivesWayToThePage(t *testing.T) {
	h := openingHarness(t)
	h.tt.Frame()
	if h.d.opening == nil || h.tt.HasText("Research") {
		t.Fatalf("at its first frame the window shows %q, want the Opening alone", h.tt.Texts())
	}
	if _, ok := h.tt.Access("zeolite is starting. Skip the Opening"); !ok {
		t.Error("the Opening has no label for whoever does not see it")
	}
	h.tt.Advance(openingLasts / 3)
	first := h.painted()
	early := append([]uint8(nil), h.tt.Image().Pix...)
	h.tt.Advance(openingLasts / 3)
	if first == 0 || h.painted() == 0 {
		t.Errorf("the Opening painted %d pixels a third of the way and %d two thirds of the way", first, h.painted())
	}
	if string(early) == string(h.tt.Image().Pix) {
		t.Error("the Opening's picture is the same a second later")
	}
	if h.d.opening == nil || h.tt.HasText("Research") {
		t.Fatal("the Opening gave way before it had lasted")
	}
	h.tt.Advance(openingLasts / 3)
	h.tt.Frame()
	if h.d.opening != nil || !h.tt.HasText("Research") {
		t.Fatalf("once the Opening had lasted the window shows %q, want the page", h.tt.Texts())
	}
	// It does not come back.
	h.tt.Advance(openingLasts)
	if h.d.opening != nil || !h.tt.HasText("Research") {
		t.Error("the Opening came back")
	}
}

// A click ends the Opening, and so does a key: Escape, or Enter, which
// presses what has the keyboard.
func TestTheOpeningIsSkipped(t *testing.T) {
	for name, skip := range map[string]func(h *harness){
		"a click": func(h *harness) { h.tt.ClickAt(640, 430) },
		"Escape":  func(h *harness) { h.tt.Key(0, ui.KeyEscape) },
		"Enter":   func(h *harness) { h.tt.Key(0, ui.KeyEnter) },
	} {
		h := openingHarness(t)
		h.tt.Frame()
		h.tt.Advance(openingLasts / 4)
		if h.d.opening == nil {
			t.Fatalf("%s: the Opening was gone before anything was done", name)
		}
		skip(h)
		h.tt.Frame()
		if h.d.opening != nil || !h.tt.HasText("Research") {
			t.Errorf("after %s the window shows %q, want the page", name, h.tt.Texts())
		}
	}
}

// Whoever asked their Mac for less motion is shown no Opening, and a
// dashboard nobody told to show one shows none.
func TestTheOpeningIsNotShownWhereItIsNotWanted(t *testing.T) {
	h := openingHarness(t)
	h.tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	h.tt.Frame()
	if h.d.opening != nil || !h.tt.HasText("Research") {
		t.Errorf("with less motion asked for, the window shows %q, want the page", h.tt.Texts())
	}
	plain := newHarness(t, []sandbox.Config{gated("s1", "Research")})
	plain.tt.Frame()
	if plain.d.opening != nil || !plain.tt.HasText("Research") {
		t.Errorf("a dashboard that was not told to shows %q, want the page", plain.tt.Texts())
	}
}

// The Opening's pictures, for whoever changes how it looks: saved where
// ZEOLITE_SHOTS says, and looked at.
func TestTheOpeningsPictures(t *testing.T) {
	if os.Getenv("ZEOLITE_SHOTS") == "" {
		t.Skip("ZEOLITE_SHOTS names no folder to save the pictures in")
	}
	h := openingHarness(t)
	h.tt.SetScale(2)
	h.tt.Frame()
	for i, at := range []time.Duration{200, 1000, 1000, 1000} {
		h.tt.Advance(at * time.Millisecond)
		h.capture(fmt.Sprintf("opening-%d", i+1))
	}
}
