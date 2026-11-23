package dashboard

import (
	"math"
	"time"

	"github.com/egoist/mygo/ui"

	"zeolite/internal/sandbox"
)

// The Opening is what the window shows when the app starts: the cage of
// a zeolite's framework, turning, with the glyphs of the agents the app
// knows moving about inside it as living things do. It is drawn as the
// icon is, in characters on a grid, whose weights say how near each part
// of the cage is. It lasts openingLasts and gives way to the page; a
// click or a key ends it sooner, and it is not shown at all to whoever
// asked their Mac for less motion.

// opening is the Opening while it shows.
type opening struct {
	// began is the time of its first frame, zero until then.
	began time.Time
	grid  openingGrid
	// advance is how wide a character of the font is, as a part of the
	// font's size, and shaped are the characters of the grid as the font
	// draws them at size: each measured and shaped once.
	advance float32
	shaped  map[byte]ui.Glyph
	size    float32
}

// How the Opening moves.
const (
	// openingLasts is how long the Opening shows, and openingDissolves
	// how long its characters take to come and, at its end, to go.
	openingLasts     = 3600 * time.Millisecond
	openingDissolves = 450 * time.Millisecond
	// openingTurn is the time the cage takes to turn once, and
	// openingTilt how far it leans towards whoever looks, in radians.
	openingTurn = 14 * time.Second
	openingTilt = 0.38
	// openingEye is how far whoever looks stands from the cage's middle,
	// in the cage's own measure: what is near is drawn larger.
	openingEye = 12.0
	// openingFrameRate is how many frames a second the Opening draws.
	openingFrameRate = 30
)

// openingName is the app's name, under the cage, a space between its
// letters.
const openingName = "z e o l i t e"

// ShowOpening has the window show the Opening before its page, from its
// next frame.
func (d *Dashboard) ShowOpening() { d.opening = &opening{} }

// openingView builds the Opening, and says whether it still shows.
func (d *Dashboard) openingView(c *ui.Context) bool {
	o := d.opening
	if o == nil {
		return false
	}
	now := c.Now()
	if o.began.IsZero() {
		o.began = now
	}
	left := openingLasts - now.Sub(o.began)
	if left <= 0 || c.Preferences().ReduceMotion {
		d.opening = nil
		return false
	}
	// The page is built when the Opening has lasted.
	c.After(left)
	end := func() { d.opening = nil }
	c.OnWindowShortcut(0, ui.KeyEscape, end)
	c.Key("opening")
	ui.ButtonBase(c).Fill().AutoFocus().Label("zeolite is starting. Skip the Opening").OnClick(end).Draw(o.paint)
	return true
}

// paint draws the Opening as it is at the frame's time.
func (o *opening) paint(p *ui.Painter, r ui.Rect) {
	p.AnimationFrameRate(openingFrameRate)
	o.grid.draw(p.Now().Sub(o.began))

	// A character is half as wide as its line is high, as in a terminal:
	// two of them side by side are a square.
	cell := min(r.W/openingColumns, r.H/(2*openingRows))
	if cell <= 0 {
		return
	}
	// The font's size is the one at which a character is as wide as a cell.
	if o.advance == 0 {
		measure := ui.Font{Family: chromeFont, Size: 100}
		o.advance = 0.6
		if shaped := ui.Shape(string(rune(glyphBoth)), measure); len(shaped) == 1 && shaped[0].Advance > 0 {
			o.advance = shaped[0].Advance / measure.Size
		}
	}
	font := ui.Font{Family: chromeFont, Size: cell / o.advance}
	if o.size != font.Size {
		o.size, o.shaped = font.Size, map[byte]ui.Glyph{}
	}
	metrics := font.Metrics()
	left := r.X + (r.W-cell*openingColumns)/2
	top := r.Y + (r.H-2*cell*openingRows)/2
	baseline := (2*cell-metrics.Ascent-metrics.Descent)/2 + metrics.Ascent

	theme := p.Theme()
	colors := [...]ui.Color{toneCage: theme.Accent, toneNear: theme.Text, toneFar: theme.TextMuted}
	var line []ui.Glyph
	for row := 0; row < openingRows; row++ {
		for tone, color := range colors {
			line = line[:0]
			for col := 0; col < openingColumns; col++ {
				c := o.grid.cells[row*openingColumns+col]
				if c.char == 0 || int(c.tone) != tone {
					continue
				}
				g, ok := o.shaped[c.char]
				if !ok {
					if shaped := ui.Shape(string(rune(c.char)), font); len(shaped) == 1 {
						g = shaped[0]
					}
					o.shaped[c.char] = g
				}
				g.X = float32(col)*cell + (cell-g.Advance)/2
				line = append(line, g)
			}
			if len(line) > 0 {
				p.Glyphs(line, left, top+float32(row)*2*cell+baseline, color)
			}
		}
	}
}

// openingTone is which of the Opening's colors a character is drawn in.
type openingTone uint8

const (
	// toneCage is the cage's, and the name's: the accent, as the icon's.
	toneCage openingTone = iota
	// toneNear is a glyph's in the near half of the cage, and toneFar
	// one's in the far half.
	toneNear
	toneFar
)

// openingCell is one character of the Opening's grid.
type openingCell struct {
	// char is the character, 0 where nothing is drawn.
	char byte
	tone openingTone
	// agent is which agent's glyph the character is a mark of, counted
	// from one in the order of the agents the app knows; 0 for none.
	agent uint8
}

// openingGrid is the Opening at one moment: its characters, and how near
// what each one shows is, so that what is near hides what is far.
type openingGrid struct {
	cells []openingCell
	depth []float64
}

// The cage is a sodalite cage, as the icon's is: a truncated octahedron.
// Its twenty-four T-atoms stand where the three coordinates are 0, 1 and
// 2 in any order and with any signs, and its thirty-six oxygen bridges
// join the atoms that are nearest each other.
var cageAtoms, cageBridges = sodaliteCage()

// cageRadius is how far a T-atom is from the cage's middle, and
// cageRoom how far its nearest face is: what is nearer the middle than
// that is inside the cage.
var (
	cageRadius = math.Sqrt(5)
	cageRoom   = math.Sqrt(3)
)

func sodaliteCage() (atoms [][3]float64, bridges [][2]int) {
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

// bridgeMarks are the characters a bridge is drawn with, from the
// farthest to the nearest: the icon's, lightest to heaviest.
const bridgeMarks = "!:;+*xX#"

// The characters of a T-atom, in the near half of the cage and in the
// far one.
const (
	atomNear = '@'
	atomFar  = '%'
)

// The characters of a glyph. A line of characters is as high as two of
// its characters are wide, so a character stands for a cell of the
// glyph's art and the cell under it: both marked, the upper alone, or
// the lower alone. A glyph of twelve cells a side is twelve characters
// by six lines, which is a square.
const (
	glyphBoth  = '#'
	glyphUpper = '"'
	glyphLower = 'm'
)

// glyphLines are a glyph's lines of characters, 0 where nothing is drawn.
func glyphLines(art glyph) [][]byte {
	marked := func(row, col int) bool { return row < len(art) && col < len(art[row]) && art[row][col] == mark }
	lines := make([][]byte, (len(art)+1)/2)
	for y := range lines {
		lines[y] = make([]byte, art.columns())
		for x := range lines[y] {
			switch upper, lower := marked(2*y, x), marked(2*y+1, x); {
			case upper && lower:
				lines[y][x] = glyphBoth
			case upper:
				lines[y][x] = glyphUpper
			case lower:
				lines[y][x] = glyphLower
			}
		}
	}
	return lines
}

// turned is a point of the cage as it is seen after the cage turned for
// a time: x to the right, y up, z towards whoever looks.
func turned(p [3]float64, elapsed time.Duration) [3]float64 {
	angle := 2 * math.Pi * elapsed.Seconds() / openingTurn.Seconds()
	sin, cos := math.Sincos(angle)
	x, y, z := p[0]*cos+p[2]*sin, p[1], -p[0]*sin+p[2]*cos
	sin, cos = math.Sincos(openingTilt)
	return [3]float64{x, y*cos - z*sin, y*sin + z*cos}
}

// place is where on the grid a point is seen, in characters across and
// lines down, each a fraction.
func place(p [3]float64) (col, row float64) {
	near := openingEye / (openingEye - p[2])
	return openingColumns/2 + p[0]*near*2*openingUnit, openingCageRow - p[1]*near*openingUnit
}

// glyphAt is where the glyph of the agent with this number is at a
// time, in the cage's measure, as it is seen: the glyphs go round the
// cage's middle one after another, against the cage's turn, as fish go
// round a bowl, each rising and falling and nearing the middle in a time
// of its own, and always inside the cage.
func glyphAt(n int, elapsed time.Duration) [3]float64 {
	t, i := elapsed.Seconds(), float64(n)
	around := 2*math.Pi*i/float64(len(sandbox.KnownAgents)) - 0.9*t + 0.25*math.Sin(1.3*t+i)
	out := 1.05 + 0.2*math.Sin(0.8*t+2*i)
	x, y, z := out*math.Cos(around), 0.75*math.Sin(1.1*t+2.4*i), out*math.Sin(around)
	sin, cos := math.Sincos(openingTilt)
	return [3]float64{x, y*cos - z*sin, y*sin + z*cos}
}

// glyphBends is how far a line of a glyph is moved to the side at a
// time, in characters: a wave runs down the glyph from its top, as down
// the body of what swims.
func glyphBends(n, line int, elapsed time.Duration) int {
	return int(math.Round(0.8 * math.Sin(2*math.Pi*(0.14*float64(line)-1.1*elapsed.Seconds())+float64(n))))
}

// shows is whether a character of the grid is drawn while only a part of
// them are: each has a share of its own, so that they come and go one by
// one.
func shows(col, row int, part float64) bool {
	h := uint32(col)*73856093 ^ uint32(row)*19349663
	h ^= h >> 13
	h *= 0x5bd1e995
	h ^= h >> 15
	return float64(h%1000)/1000 < part
}

// put draws a character where nothing nearer is drawn yet. A character
// that is 0 draws nothing, and hides what is farther.
func (g *openingGrid) put(col, row int, depth float64, c openingCell) {
	if col < 0 || col >= openingColumns || row < 0 || row >= openingRows {
		return
	}
	if i := row*openingColumns + col; depth >= g.depth[i] {
		g.cells[i], g.depth[i] = c, depth
	}
}

// draw makes the grid what the Opening is after a time.
func (g *openingGrid) draw(elapsed time.Duration) {
	if len(g.cells) == 0 {
		g.cells = make([]openingCell, openingColumns*openingRows)
		g.depth = make([]float64, openingColumns*openingRows)
	}
	for i := range g.cells {
		g.cells[i], g.depth[i] = openingCell{}, math.Inf(-1)
	}

	// The bridges, each a line of characters from one atom to the next.
	for _, bridge := range cageBridges {
		a, b := turned(cageAtoms[bridge[0]], elapsed), turned(cageAtoms[bridge[1]], elapsed)
		colA, rowA := place(a)
		colB, rowB := place(b)
		steps := int(2*math.Max(math.Abs(colB-colA), math.Abs(rowB-rowA))) + 1
		for s := 0; s <= steps; s++ {
			f := float64(s) / float64(steps)
			depth := a[2] + (b[2]-a[2])*f
			weight := int((depth + cageRadius) / (2 * cageRadius) * float64(len(bridgeMarks)))
			weight = min(max(weight, 0), len(bridgeMarks)-1)
			g.put(int(math.Round(colA+(colB-colA)*f)), int(math.Round(rowA+(rowB-rowA)*f)), depth, openingCell{char: bridgeMarks[weight]})
		}
	}
	// The atoms, each a knot two characters wide over its bridges' ends.
	for _, atom := range cageAtoms {
		p := turned(atom, elapsed)
		col, row := place(p)
		char := byte(atomNear)
		if p[2] < 0 {
			char = atomFar
		}
		for dx := -1; dx <= 0; dx++ {
			g.put(int(math.Round(col))+dx, int(math.Round(row)), p[2]+0.1, openingCell{char: char})
		}
	}
	// The glyphs, and around each a rim of nothing, which keeps the glyph
	// apart from what is behind it.
	for n, agent := range sandbox.KnownAgents {
		lines := glyphLines(agentGlyphs[agent.Executable])
		p := glyphAt(n, elapsed)
		col, row := place(p)
		left, top := int(math.Round(col))-len(lines[0])/2, int(math.Round(row))-len(lines)/2
		tone := toneNear
		if p[2] < 0 {
			tone = toneFar
		}
		for pass := 0; pass < 2; pass++ {
			for y, line := range lines {
				bend := glyphBends(n, y, elapsed)
				for x, char := range line {
					if char == 0 {
						continue
					}
					at := left + x + bend
					if pass == 0 {
						for dy := -1; dy <= 1; dy++ {
							for dx := -1; dx <= 1; dx++ {
								g.put(at+dx, top+y+dy, p[2]-0.01, openingCell{})
							}
						}
						continue
					}
					g.put(at, top+y, p[2], openingCell{char: char, tone: tone, agent: uint8(n + 1)})
				}
			}
		}
	}
	// The app's name, under the cage.
	for i := 0; i < len(openingName); i++ {
		if openingName[i] != ' ' {
			g.put((openingColumns-len(openingName))/2+i, openingNameRow, math.Inf(1), openingCell{char: openingName[i]})
		}
	}

	// The characters come one by one as the Opening begins, and go one
	// by one as it ends.
	part := math.Min(elapsed.Seconds(), (openingLasts-elapsed).Seconds()) / openingDissolves.Seconds()
	if part < 1 {
		for i := range g.cells {
			if !shows(i%openingColumns, i/openingColumns, part) {
				g.cells[i] = openingCell{}
			}
		}
	}
}
