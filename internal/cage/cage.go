// Package cage is the sodalite cage that the app's pictures turn: the
// unit a zeolite's framework is built of, as it is seen after it turned
// for a time, and as characters on a grid whose weights say how near
// each part of it is. The Opening turns it in the window and the Masthead
// beside the app's name; the package knows neither, and is given where
// on a grid the cage stands.
package cage

import (
	"math"
	"time"
)

// How the cage turns and how it is seen.
const (
	// Turn is the time the cage takes to turn once, and Tilt how far it
	// leans towards whoever looks, in radians.
	Turn = 14 * time.Second
	Tilt = 0.38
	// Eye is how far whoever looks stands from the cage's middle, in the
	// cage's own measure: what is near is drawn larger.
	Eye = 12.0
	// Again is the time after which the cage looks as it did: a quarter
	// of a turn, the cage being the same seen from each of four sides.
	Again = Turn / 4
)

// The cage is a truncated octahedron. Its twenty-four T-atoms stand
// where the three coordinates are 0, 1 and 2 in any order and with any
// signs, and its thirty-six oxygen bridges join the atoms that are
// nearest each other.
var Atoms, Bridges = sodalite()

// Radius is how far a T-atom is from the cage's middle, and Room how far
// its nearest face is: what is nearer the middle than that is inside the
// cage.
var (
	Radius = math.Sqrt(5)
	Room   = math.Sqrt(3)
)

func sodalite() (atoms [][3]float64, bridges [][2]int) {
	for none := 0; none < 3; none++ {
		for _, one := range []int{(none + 1) % 3, (none + 2) % 3} {
			two := 3 - none - one
			for _, a := range []float64{-1, 1} {
				for _, b := range []float64{-2, 2} {
					var atom [3]float64
					atom[one], atom[two] = a, b
					atoms = append(atoms, atom)
				}
			}
		}
	}
	for i, a := range atoms {
		for j := i + 1; j < len(atoms); j++ {
			b := atoms[j]
			if d := (a[0]-b[0])*(a[0]-b[0]) + (a[1]-b[1])*(a[1]-b[1]) + (a[2]-b[2])*(a[2]-b[2]); d < 2.01 {
				bridges = append(bridges, [2]int{i, j})
			}
		}
	}
	return atoms, bridges
}

// Marks are the characters a bridge is drawn with, from the farthest to
// the nearest: the icon's, lightest to heaviest.
const Marks = "!:;+*xX#"

// The characters of a T-atom, in the near half of the cage and in the
// far one.
const (
	AtomNear = '@'
	AtomFar  = '%'
)

// Turned is a point of the cage as it is seen after the cage turned for
// a time: x to the right, y up, z towards whoever looks.
func Turned(p [3]float64, elapsed time.Duration) [3]float64 {
	sin, cos := math.Sincos(2 * math.Pi * elapsed.Seconds() / Turn.Seconds())
	return Leaned([3]float64{p[0]*cos + p[2]*sin, p[1], -p[0]*sin + p[2]*cos})
}

// Leaned is a point as it is seen with the cage leaning towards whoever
// looks.
func Leaned(p [3]float64) [3]float64 {
	sin, cos := math.Sincos(Tilt)
	return [3]float64{p[0], p[1]*cos - p[2]*sin, p[1]*sin + p[2]*cos}
}

// Grid is where a cage stands on a grid of characters, each half as wide
// as its line is high: the character and the line of its middle, and how
// many lines one unit of the cage's own measure takes, in which its
// T-atoms stand at 0, 1 and 2.
type Grid struct {
	Column, Row, Unit float64
}

// Place is where on the grid a point is seen, in characters across and
// lines down, each a fraction.
func (g Grid) Place(p [3]float64) (col, row float64) {
	near := Eye / (Eye - p[2])
	return g.Column + p[0]*near*2*g.Unit, g.Row - p[1]*near*g.Unit
}

// Draw gives put the characters of the cage after it turned for a time,
// each with the character and line it stands at and how near what it
// shows is: its bridges, each a line of characters from one atom to the
// next, and then its atoms, each a knot two characters wide over its
// bridges' ends. Where two stand at one place, the nearer is the one to
// keep.
func (g Grid) Draw(elapsed time.Duration, put func(col, row int, depth float64, char byte)) {
	for _, bridge := range Bridges {
		a, b := Turned(Atoms[bridge[0]], elapsed), Turned(Atoms[bridge[1]], elapsed)
		colA, rowA := g.Place(a)
		colB, rowB := g.Place(b)
		steps := int(2*math.Max(math.Abs(colB-colA), math.Abs(rowB-rowA))) + 1
		for s := 0; s <= steps; s++ {
			f := float64(s) / float64(steps)
			depth := a[2] + (b[2]-a[2])*f
			weight := int((depth + Radius) / (2 * Radius) * float64(len(Marks)))
			weight = min(max(weight, 0), len(Marks)-1)
			put(int(math.Round(colA+(colB-colA)*f)), int(math.Round(rowA+(rowB-rowA)*f)), depth, Marks[weight])
		}
	}
	for _, atom := range Atoms {
		p := Turned(atom, elapsed)
		col, row := g.Place(p)
		char := byte(AtomNear)
		if p[2] < 0 {
			char = AtomFar
		}
		for dx := -1; dx <= 0; dx++ {
			put(int(math.Round(col))+dx, int(math.Round(row)), p[2]+0.1, char)
		}
	}
}
