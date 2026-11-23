package main

import (
	"bytes"
	"image/gif"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"zeolite/internal/cage"
	"zeolite/internal/chargrid"
)

// The Masthead the README shows is the one this program draws: run
// `go generate ./cmd/masthead` after changing its art, the cage or the
// palette.
func TestCommittedMastheadIsTheOneDrawn(t *testing.T) {
	committed, err := os.ReadFile("../../docs/masthead.gif")
	if err != nil {
		t.Fatal(err)
	}
	drawn, err := masthead()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, drawn) {
		t.Errorf("docs/masthead.gif, of %d bytes, is not the Masthead drawn, of %d: run go generate ./cmd/masthead", len(committed), len(drawn))
	}
}

// The Masthead turns without end and without a jump: its pictures last,
// together, as long as the cage takes to look as it did, and it begins
// again for ever. Its first picture is the whole of it, and the ones
// after it only what changed.
func TestTheMastheadShowsTheCageTurningWithoutEnd(t *testing.T) {
	data, err := masthead()
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if g.LoopCount != 0 {
		t.Errorf("the Masthead is shown %d times over, want for ever", g.LoopCount)
	}
	lasts := 0
	for _, hundredths := range g.Delay {
		lasts += hundredths
	}
	if got := time.Duration(lasts) * 10 * time.Millisecond; len(g.Image) != frames || got != cage.Again {
		t.Errorf("%d pictures last %v, and the cage looks as it did after %v", len(g.Image), got, cage.Again)
	}
	if first := g.Image[0].Bounds(); first.Dx() != g.Config.Width || first.Dy() != g.Config.Height {
		t.Errorf("the first picture is %v of a Masthead %d by %d", first, g.Config.Width, g.Config.Height)
	}
	whole := g.Config.Width * g.Config.Height
	for n, frame := range g.Image[1:] {
		drawn := 0
		for _, index := range frame.Pix {
			if index != 0 {
				drawn++
			}
		}
		if drawn == 0 || drawn > whole/10 {
			t.Errorf("picture %d draws %d pixels of %d", n+2, drawn, whole)
		}
	}
	// It weighs what a README can carry.
	if len(data) > 1<<20 {
		t.Errorf("the Masthead is %d KB", len(data)>>10)
	}
}

// Each character of the name trembles about its place: by whole pixels
// and never far, in a way of its own, and back where it began when the
// cage looks as it did. The cage's characters stand in their cells.
func TestTheNameTrembles(t *testing.T) {
	lines := grid(0)
	ways := map[[2 * frames]float64]bool{}
	characters := 0
	for row, line := range lines {
		for col := nameColumn; col < len(line); col++ {
			if line[col] == ' ' {
				continue
			}
			characters++
			var way [2 * frames]float64
			moved := false
			for n := 0; n < frames; n++ {
				dx, dy := tremble(col, row, cage.Again*time.Duration(n)/frames)
				if dx != math.Round(dx) || dy != math.Round(dy) || math.Abs(dx) > math.Round(trembleBy) || math.Abs(dy) > math.Round(trembleBy) {
					t.Fatalf("the character at column %d, line %d stands %v, %v from its place", col, row, dx, dy)
				}
				way[2*n], way[2*n+1] = dx, dy
				moved = moved || dx != way[0] || dy != way[1]
			}
			if !moved {
				t.Errorf("the character at column %d, line %d never moves", col, row)
			}
			ways[way] = true
			// Just before the cage looks as it did, and just after, the
			// character is where it was just before and after the beginning.
			for _, by := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
				ax, ay := tremble(col, row, by)
				bx, by := tremble(col, row, cage.Again+by)
				if ax != bx || ay != by {
					t.Fatalf("the character at column %d, line %d begins again at %v, %v from %v, %v", col, row, bx, by, ax, ay)
				}
			}
		}
	}
	if characters == 0 || len(ways) < characters*9/10 {
		t.Errorf("%d characters of the name move in %d ways", characters, len(ways))
	}

	// In the picture, what trembles is the name, and nothing of the cage.
	face, err := chargrid.NewFace(cellWidth)
	if err != nil {
		t.Fatal(err)
	}
	first, later := picture(face, lines, 0), picture(face, lines, cage.Again/7)
	cageEnds := (marginColumns + cageColumns) * cellWidth
	name := false
	for y := 0; y < first.Bounds().Dy(); y++ {
		for x := 0; x < first.Bounds().Dx(); x++ {
			if first.AlphaAt(x, y) == later.AlphaAt(x, y) {
				continue
			}
			if x < cageEnds {
				t.Fatalf("the pixel at %d, %d, in the cage's room, changed while the cage did not turn", x, y)
			}
			name = true
		}
	}
	if !name {
		t.Error("the name's pixels are the same a while later")
	}
}

// The grid holds the cage, whole and turning, and the app's name beside
// it, the same at every moment; and the name's letters are well formed.
func TestTheMastheadsGrid(t *testing.T) {
	for letter, art := range letters {
		if len(art) != letterRows {
			t.Errorf("%q is %d lines high, want %d", letter, len(art), letterRows)
		}
		marks := 0
		for i, line := range art {
			if len(line) != len(art[0]) {
				t.Errorf("%q: line %d is %d cells wide, the first %d", letter, i+1, len(line), len(art[0]))
			}
			if rest := strings.Trim(line, string(mark)+"."); rest != "" {
				t.Errorf("%q: line %d holds %q, which is neither a mark nor nothing", letter, i+1, rest)
			}
			marks += strings.Count(line, string(mark))
		}
		if marks == 0 {
			t.Errorf("%q has no mark", letter)
		}
	}
	for i := 0; i < len(name); i++ {
		if letters[name[i]] == nil {
			t.Errorf("the name's %q has no art", name[i])
		}
	}

	// The name is italic: its lines are of one length, the lowest stands
	// where its art has it, and every leanRows lines above it stand one
	// character further to the right. The l alone reaches the highest.
	word := nameLines()
	before := 0
	for i := 0; name[i] != 'l'; i++ {
		before += len(letters[name[i]][0]) + letterGap
	}
	for row, line := range word {
		if len(line) != len(word[0]) {
			t.Errorf("line %d of the name is %d characters long, the first %d", row+1, len(line), len(word[0]))
		}
		want := before + (letterRows-1-row)/leanRows
		if got := strings.Index(line[before:], string(rune(cage.AtomNear))) + before; row < 6 && got != want {
			t.Errorf("line %d of the name begins its l at character %d, want %d", row+1, got, want)
		}
	}
	if low, high := word[letterRows-1], word[0]; low[0] == ' ' || lean(0) < 4 || strings.TrimSpace(high) != strings.Repeat(string(rune(cage.AtomNear)), len(letters['l'][0])) {
		t.Errorf("the name's lowest line is %q and its highest %q, which leans by %d", low, high, lean(0))
	}

	parts := func(at time.Duration) (cagePart, namePart string) {
		for _, line := range grid(at) {
			cagePart += string(line[:marginColumns+cageColumns]) + "\n"
			namePart += string(line[marginColumns+cageColumns:]) + "\n"
		}
		return cagePart, namePart
	}
	firstCage, firstName := parts(0)
	if strings.TrimSpace(firstName) == "" {
		t.Fatal("the grid holds no name")
	}
	where := cage.Grid{Column: marginColumns + cageColumns/2, Row: marginRows + cageRows/2, Unit: cageUnit}
	for n := 0; n < frames; n++ {
		at := cage.Again * time.Duration(n) / frames
		cagePart, namePart := parts(at)
		if namePart != firstName {
			t.Fatalf("after %v the name is not what it was", at)
		}
		if n > 0 && cagePart == firstCage {
			t.Errorf("after %v the cage is as it was at first", at)
		}
		for _, char := range []byte{cage.AtomNear, cage.AtomFar, cage.Marks[0], cage.Marks[len(cage.Marks)-1]} {
			if !strings.ContainsRune(cagePart, rune(char)) {
				t.Errorf("after %v the cage has no %q", at, char)
			}
		}
		// The cage is whole, in its room: no atom is outside it, or in
		// the name's.
		for i, atom := range cage.Atoms {
			col, row := where.Place(cage.Turned(atom, at))
			if col < marginColumns || col > marginColumns+cageColumns-1 || row < marginRows || row > marginRows+cageRows-1 {
				t.Fatalf("after %v T-atom %d is at column %.0f, line %.0f: outside the cage's room", at, i, col, row)
			}
		}
	}
}
