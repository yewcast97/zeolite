package cage

import (
	"go/parser"
	"go/token"
	"math"
	"strings"
	"testing"
	"time"
)

// The cage is a sodalite cage: twenty-four T-atoms, each at the same
// distance from the middle and bridged to three others, by thirty-six
// bridges in all.
func TestTheCageIsASodaliteCage(t *testing.T) {
	if len(Atoms) != 24 || len(Bridges) != 36 {
		t.Fatalf("the cage has %d T-atoms and %d bridges, want 24 and 36", len(Atoms), len(Bridges))
	}
	bridged := map[int]int{}
	for _, b := range Bridges {
		bridged[b[0]]++
		bridged[b[1]]++
	}
	for i, a := range Atoms {
		if r := math.Sqrt(a[0]*a[0] + a[1]*a[1] + a[2]*a[2]); math.Abs(r-Radius) > 1e-9 {
			t.Errorf("T-atom %d is %.3f from the middle, want %.3f", i, r, Radius)
		}
		if bridged[i] != 3 {
			t.Errorf("T-atom %d has %d bridges, want 3", i, bridged[i])
		}
	}
}

// drawn is the cage on a grid after a time, as lines of characters: the
// nearer of two that stand at one place.
func drawn(g Grid, columns, rows int, elapsed time.Duration) string {
	chars, depth := make([]byte, columns*rows), make([]float64, columns*rows)
	for i := range chars {
		chars[i], depth[i] = ' ', math.Inf(-1)
	}
	g.Draw(elapsed, func(col, row int, d float64, char byte) {
		if col < 0 || col >= columns || row < 0 || row >= rows {
			chars = nil
			return
		}
		if i := row*columns + col; chars != nil && d >= depth[i] {
			chars[i], depth[i] = char, d
		}
	})
	if chars == nil {
		return "off the grid"
	}
	var b strings.Builder
	for row := 0; row < rows; row++ {
		b.Write(chars[row*columns : (row+1)*columns])
		b.WriteByte('\n')
	}
	return b.String()
}

// The cage turns, a moment of it is the same however often it is drawn,
// and after a quarter of a turn it looks as it did: what shows it over
// that time and begins again shows it turning without end. Its marks say
// how near each part is, from the lightest to the heaviest, and both
// kinds of atom are seen.
func TestTheCageTurnsAndLooksTheSameAfterAQuarterTurn(t *testing.T) {
	g := Grid{Column: 30, Row: 15, Unit: 5}
	for _, at := range []time.Duration{0, 400 * time.Millisecond, 1900 * time.Millisecond, Again - 50*time.Millisecond} {
		now := drawn(g, 60, 30, at)
		if now == "off the grid" {
			t.Fatalf("after %v the cage is off a grid it has room on", at)
		}
		if drawn(g, 60, 30, at) != now {
			t.Errorf("the moment after %v was drawn two ways", at)
		}
		if drawn(g, 60, 30, at+100*time.Millisecond) == now {
			t.Errorf("the cage is the same a tenth of a second after %v", at)
		}
		// But for a character that stands between two places, and falls
		// to the other for a difference no eye sees.
		again := drawn(g, 60, 30, at+Again)
		differing := 0
		for i := range now {
			if now[i] != again[i] {
				differing++
			}
		}
		if differing > 4 {
			t.Errorf("a quarter of a turn after %v the cage differs in %d characters:\n%s\n%s", at, differing, now, again)
		}
		for _, char := range []byte{AtomNear, AtomFar, Marks[0], Marks[len(Marks)-1]} {
			if !strings.ContainsRune(now, rune(char)) {
				t.Errorf("after %v the cage has no %q", at, char)
			}
		}
	}
}

// The cage is read by the window and by the Masthead's generator alike: it
// imports nothing of the app's and nothing that draws.
func TestTheCageImportsNothing(t *testing.T) {
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			files++
			for _, imp := range file.Imports {
				if path := strings.Trim(imp.Path.Value, `"`); strings.Contains(path, ".") || strings.HasPrefix(path, "zeolite/") {
					t.Errorf("%s imports %s", name, path)
				}
			}
		}
	}
	if files < 2 {
		t.Errorf("read %d files: the test is not reading the package", files)
	}
}
