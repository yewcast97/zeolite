package palette

import (
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"
	"testing"
)

// The palette is read by the window, the dashboard and the icon's
// generator alike: it imports nothing of the app's and nothing that
// draws.
func TestThePaletteImportsNothing(t *testing.T) {
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			files++
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if strings.HasSuffix(name, "_test.go") && !strings.Contains(path, ".") && !strings.HasPrefix(path, "zeolite") {
					continue // a test may use the standard library
				}
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
	if files < 2 {
		t.Errorf("read %d files: the test is not reading the package", files)
	}
}

// luminance is the relative luminance of a color, as WCAG defines it.
func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	if len(hex) != 7 || hex[0] != '#' {
		t.Fatalf("%q is not a color written as #rrggbb", hex)
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		t.Fatalf("%q is not a color: %v", hex, err)
	}
	channel := func(c uint64) float64 {
		s := float64(c&0xff) / 255
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(v>>16) + 0.7152*channel(v>>8) + 0.0722*channel(v)
}

// contrast is the contrast ratio of two colors, from 1 to 21.
func contrast(t *testing.T, a, b string) float64 {
	la, lb := luminance(t, a), luminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Text is read against what is behind it: every pair the window uses has
// the contrast body text needs, 4.5 to 1.
func TestTextStandsOutFromWhatIsBehindIt(t *testing.T) {
	pairs := []struct {
		what, text, behind string
		least              float64
	}{
		{"text on the background", Text, Background, 7},
		{"text on a panel", Text, Panel, 7},
		{"text on a control", Text, Raised, 4.5},
		{"text on a control under the pointer", Text, RaisedHover, 4.5},
		{"text on a pressed control", Text, RaisedPressed, 4.5},
		{"secondary text on the background", Muted, Background, 4.5},
		{"secondary text on a panel", Muted, Panel, 4.5},
		{"secondary text on a control", Muted, Raised, 4.5},
		{"text on the accent", AccentText, Accent, 4.5},
		{"text on the accent under the pointer", AccentText, AccentHover, 4.5},
		{"text on the pressed accent", AccentText, AccentPressed, 4.5},
		{"text on an open gate's color", OnStatus, Success, 4.5},
		{"text on a starting sandbox's color", OnStatus, Warning, 4.5},
		{"text on a locked gate's color", OnStatus, Danger, 4.5},
		{"text on the Attention mark", OnStatus, Attention, 4.5},
		{"the Attention mark on a panel", Attention, Panel, 4.5},
		{"the Attention mark on the background", Attention, Background, 4.5},
		{"the Attention mark on a chosen row", Attention, RaisedPressed, 3},
		{"selected text", Text, Selection, 4.5},
		{"what went wrong, as text on the background", Danger, Background, 4.5},
		{"the accent, as a link or a caret, on the background", Accent, Background, 4.5},
	}
	// A terminal's colors are read on its background, but for black,
	// which is there to be a background.
	for i, c := range Terminal {
		if i == 0 {
			continue
		}
		least := 4.5
		if i == 8 {
			least = 3 // bright black: dimmed text
		}
		pairs = append(pairs, struct {
			what, text, behind string
			least              float64
		}{"the terminal's color " + strconv.Itoa(i), c, Background, least})
	}
	for _, p := range pairs {
		if got := contrast(t, p.text, p.behind); got < p.least {
			t.Errorf("%s: %s on %s has a contrast of %.2f, want at least %.1f", p.what, p.text, p.behind, got, p.least)
		}
	}
}

// The theme is soft: nothing is pure black or pure white, and the
// window's layers step up gently from back to front.
func TestThePaletteIsSoft(t *testing.T) {
	for _, c := range []string{Background, Panel, Raised, Border, Text, Muted} {
		if l := luminance(t, c); l < 0.015 || l > 0.85 {
			t.Errorf("%s has a luminance of %.3f: harsher than a soft dark theme is", c, l)
		}
	}
	// The colors that mean something are neither dim nor glaring, and the
	// Attention mark is told from a starting sandbox's color and from a
	// locked gate's by more than its place.
	for _, c := range []string{Success, Warning, Danger, Attention} {
		if l := luminance(t, c); l < 0.25 || l > 0.6 {
			t.Errorf("%s has a luminance of %.3f: a status color is neither dim nor glaring", c, l)
		}
	}
	for _, other := range []string{Warning, Danger} {
		if Attention == other {
			t.Errorf("the Attention mark has the color %s of another status", other)
		}
	}
	layers := []string{Background, Panel, Raised, RaisedHover, RaisedPressed}
	for i := 1; i < len(layers); i++ {
		if step := contrast(t, layers[i], layers[i-1]); luminance(t, layers[i]) <= luminance(t, layers[i-1]) || step > 1.35 {
			t.Errorf("from %s to %s the window steps by %.2f, want a gentle step up", layers[i-1], layers[i], step)
		}
	}
}
