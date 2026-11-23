package main

import (
	"image/png"
	"os"
	"testing"

	"zeolite/internal/palette"
)

// The icon the app is built with is the one this program draws: run
// `go generate ./cmd/appicon` after changing the art or the palette.
func TestCommittedIconIsTheOneDrawn(t *testing.T) {
	f, err := os.Open("../../resources/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	committed, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	drawn, err := icon()
	if err != nil {
		t.Fatal(err)
	}
	if committed.Bounds() != drawn.Bounds() {
		t.Fatalf("resources/icon.png is %v, the icon drawn %v", committed.Bounds(), drawn.Bounds())
	}
	differing := 0
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if r1, g1, b1, a1 := committed.At(x, y).RGBA(); func() bool {
				r2, g2, b2, a2 := drawn.At(x, y).RGBA()
				return r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2
			}() {
				differing++
			}
		}
	}
	if differing > 0 {
		t.Errorf("resources/icon.png differs from the icon drawn in %d pixels: run go generate ./cmd/appicon", differing)
	}
}

// The cage is drawn in characters: its lines are all as wide, hold
// printable ASCII alone, and say how near each part is with marks of
// several weights.
func TestCageIsDrawnInCharacters(t *testing.T) {
	lines := rows()
	if len(lines) < 10 {
		t.Fatalf("the art has %d lines", len(lines))
	}
	marks, kinds := 0, map[rune]bool{}
	for i, line := range lines {
		if len(line) != len(lines[0]) {
			t.Errorf("line %d is %d characters wide, the first %d", i+1, len(line), len(lines[0]))
		}
		for _, r := range line {
			switch {
			case r <= ' ' || r > '~':
				t.Errorf("line %d holds %q, which is not a printable ASCII character", i+1, r)
			case r != nothing:
				marks++
				kinds[r] = true
			}
		}
	}
	// A framework, not a block and not a few strokes: between a fifth
	// and a half of the picture.
	if cells := len(lines) * len(lines[0]); marks*5 < cells || marks*2 > cells {
		t.Errorf("%d of %d cells are marks", marks, cells)
	}
	if len(kinds) < 6 {
		t.Errorf("the cage is drawn with %d characters, want at least 6", len(kinds))
	}
}

// The cage has its twenty-four T-atoms, each a knot of one mark that no
// edge is drawn with: twelve in the near half, twelve in the far half.
func TestCageHasItsTAtoms(t *testing.T) {
	lines := rows()
	knots := func(mark byte) int {
		seen := map[[2]int]bool{}
		count := 0
		for y := range lines {
			for x := range lines[y] {
				if lines[y][x] != mark || seen[[2]int{y, x}] {
					continue
				}
				// One knot: the marks that touch, corners included.
				count++
				seen[[2]int{y, x}] = true
				for stack := [][2]int{{y, x}}; len(stack) > 0; {
					at := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					for dy := -1; dy <= 1; dy++ {
						for dx := -1; dx <= 1; dx++ {
							next := [2]int{at[0] + dy, at[1] + dx}
							if next[0] < 0 || next[0] >= len(lines) || next[1] < 0 || next[1] >= len(lines[next[0]]) {
								continue
							}
							if lines[next[0]][next[1]] == mark && !seen[next] {
								seen[next] = true
								stack = append(stack, next)
							}
						}
					}
				}
			}
		}
		return count
	}
	if near, far := knots('@'), knots('%'); near != 12 || far != 12 {
		t.Errorf("the cage has %d T-atoms in its near half and %d in its far half, want 12 and 12", near, far)
	}
}

// The icon is the palette's: its square is the panel's color, its cage
// the accent's, and its corners are clear.
func TestIconIsDrawnInThePalette(t *testing.T) {
	img, err := icon()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, a := img.At(4, 4).RGBA(); a != 0 {
		t.Error("the icon's corner is not clear")
	}
	panel, accent := rgb(palette.Panel), rgb(palette.Accent)
	if got := img.NRGBAAt(margin+radius, margin+20); got != panel {
		t.Errorf("the square is %v, want the panel's %v", got, panel)
	}
	found := false
	for y := 0; y < size && !found; y++ {
		for x := 0; x < size; x++ {
			if img.NRGBAAt(x, y) == accent {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("no pixel of the icon is the accent's color")
	}
}
