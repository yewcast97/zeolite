// Command masthead draws the Masthead, the picture at the top of the
// README: the cage of a zeolite's framework, turning as it does in the
// Opening, and beside it the app's name in italic letters made of
// characters, each of which trembles about its place, all on a grid of
// characters in the palette's colors. It is a picture that moves: the
// cage looks the same after a quarter of a turn, the name is by then
// as it was, and the Masthead shows that quarter over and over.
//
//	masthead <file.gif>
package main

//go:generate go run . ../../docs/masthead.gif

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"zeolite/internal/cage"
	"zeolite/internal/chargrid"
	"zeolite/internal/palette"
)

// letters are the art of the app's name: its letters, standing, each a
// grid in which a mark is a character that is drawn and a dot one that
// is not. A line of characters is twice as high as a character is wide:
// a stroke that stands is three characters wide and one that lies is
// one line high, which is a little thinner.
var letters = map[byte][]string{
	'z': {
		"................",
		"................",
		"................",
		"................",
		"................",
		"................",
		"################",
		"............####",
		"...........####.",
		".........####...",
		"........####....",
		".......####.....",
		".....####.......",
		"....####........",
		"...####.........",
		".####...........",
		"####............",
		"################",
	},
	'e': {
		"....................",
		"....................",
		"....................",
		"....................",
		"....................",
		"....................",
		"......########......",
		"...#####....#####...",
		".####..........####.",
		"###..............###",
		"###..............###",
		"####################",
		"###.................",
		"###.................",
		"###.................",
		".####..........####.",
		"...#####....#####...",
		"......########......",
	},
	'o': {
		"....................",
		"....................",
		"....................",
		"....................",
		"....................",
		"....................",
		"......########......",
		"...#####....#####...",
		".####..........####.",
		"###..............###",
		"###..............###",
		"###..............###",
		"###..............###",
		"###..............###",
		"###..............###",
		".####..........####.",
		"...#####....#####...",
		"......########......",
	},
	'l': {
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
	},
	'i': {
		"...",
		"...",
		"###",
		"###",
		"...",
		"...",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
		"###",
	},
	't': {
		"..............",
		"..............",
		"....###.......",
		"....###.......",
		"....###.......",
		"....###.......",
		"##############",
		"....###.......",
		"....###.......",
		"....###.......",
		"....###.......",
		"....###.......",
		"....###.......",
		"....###.......",
		"....###.......",
		"....###.....##",
		".....###...###",
		"......#######.",
	},
}

// name is the app's name, and mark the character of the letters' art
// that is drawn.
const (
	name = "zeolite"
	mark = '#'
)

// The Masthead's measures, in characters of its grid and lines of them.
const (
	// marginColumns and marginRows are the room around what is drawn.
	marginColumns = 6
	marginRows    = 2
	// cageColumns and cageRows are the room the cage turns in, and
	// cageUnit how many lines one unit of the cage's own measure takes.
	cageColumns = 58
	cageRows    = 29
	cageUnit    = 5
	// gapColumns is the room between the cage and the name, and
	// letterGap the room between two letters.
	gapColumns = 8
	letterGap  = 5
	// letterRows is how high a letter's art is, in lines, and leanRows
	// how many lines the name rises by for each character it leans to
	// the right: its letters are italic.
	letterRows = 18
	leanRows   = 2
)

// The Masthead's measures in pixels.
const (
	// cellWidth is the width of a character: a line is twice as high.
	cellWidth = 8
	// radius is that of the Masthead's corners.
	radius = 28
)

// How the Masthead moves: it shows frames pictures, each for delay
// hundredths of a second, which together last as long as the cage takes
// to look as it did.
const (
	frames = 50
	delay  = 7
)

// How the name trembles: each of its characters moves about its place
// as an atom does about its place in a framework, each in a way of its
// own. It moves by whole pixels, so that a picture holds only the
// characters that moved, and by no more than trembleBy of them either
// way, rounded, which leaves the letters as they read. A character's
// movement is waves that each fit a whole number of times, from
// trembleSlowest to trembleFastest, in the time the cage takes to look
// as it did, so that the name too is then as it was.
const (
	trembleBy      = 1.5
	trembleSlowest = 3
	trembleFastest = 7
)

// shades is how many colors stand between the Masthead's ground and its
// characters, both among them: the edges of the characters are of the
// ones between.
const shades = 8

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: masthead <file.gif>")
		os.Exit(2)
	}
	data, err := masthead()
	if err == nil {
		err = os.WriteFile(os.Args[1], data, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "masthead:", err)
		os.Exit(1)
	}
}

// rgb reads a color of the palette.
func rgb(hex string) color.NRGBA {
	v, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	if err != nil || len(hex) != 7 {
		panic("masthead: " + hex + " is not a color of the palette")
	}
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}
}

// lean is how many characters to the right a line of the name stands of
// where its art has it: none for the lowest line, and one more for every
// leanRows lines above it.
func lean(row int) int { return (letterRows - 1 - row) / leanRows }

// nameLines are the lines of the app's name: its letters' art side by
// side, a character where a letter has a mark and a space elsewhere,
// each line leaning as far as its height asks. The lines are of one
// length.
func nameLines() []string {
	lines := make([]string, letterRows)
	for row := range lines {
		lines[row] = strings.Repeat(" ", lean(row))
		for i := 0; i < len(name); i++ {
			if i > 0 {
				lines[row] += strings.Repeat(" ", letterGap)
			}
			for _, cell := range letters[name[i]][row] {
				if cell == mark {
					lines[row] += string(rune(cage.AtomNear))
				} else {
					lines[row] += " "
				}
			}
		}
		lines[row] += strings.Repeat(" ", lean(0)-lean(row))
	}
	return lines
}

// grid is the Masthead's characters at a moment, a space where nothing is
// drawn: the cage after it turned for that long, and the name.
func grid(elapsed time.Duration) [][]byte {
	word := nameLines()
	columns := 2*marginColumns + cageColumns + gapColumns + len(word[0])
	rows := 2*marginRows + cageRows
	lines := make([][]byte, rows)
	for row := range lines {
		lines[row] = bytes.Repeat([]byte{' '}, columns)
	}
	// The cage: of two characters at one place, the nearer.
	depth := make([]float64, columns*rows)
	for i := range depth {
		depth[i] = math.Inf(-1)
	}
	at := cage.Grid{Column: marginColumns + cageColumns/2, Row: marginRows + cageRows/2, Unit: cageUnit}
	at.Draw(elapsed, func(col, row int, d float64, char byte) {
		if col < 0 || col >= columns || row < 0 || row >= rows {
			return
		}
		if i := row*columns + col; d >= depth[i] {
			lines[row][col], depth[i] = char, d
		}
	})
	// The name, in the middle of the Masthead's height.
	top := (rows - len(word)) / 2
	for row, line := range word {
		copy(lines[top+row][nameColumn:], line)
	}
	return lines
}

// nameColumn is the column of the grid at which the name begins: what
// is drawn from there on is the name's.
const nameColumn = marginColumns + cageColumns + gapColumns

// tremble is how far from its place the name's character at a column
// and a line of the grid stands at a moment, in pixels to the right and
// down.
func tremble(col, row int, elapsed time.Duration) (dx, dy float64) {
	// The character's own numbers, one after another, each from 0 to 1.
	h := uint32(col)*73856093 ^ uint32(row)*19349663
	own := func() float64 {
		h ^= h >> 13
		h *= 0x5bd1e995
		h ^= h >> 15
		return float64(h%4096) / 4096
	}
	turn := 2 * math.Pi * float64(elapsed) / float64(cage.Again)
	// Two waves each way, the second lighter and of another number of
	// times: no character goes round and round one path, and none has
	// waves that undo each other and leave it still.
	const kinds = trembleFastest - trembleSlowest + 1
	way := func() float64 {
		first := math.Floor(own() * kinds)
		second := math.Mod(first+1+math.Floor(own()*(kinds-1)), kinds)
		return 0.6*math.Sin((trembleSlowest+first)*turn+2*math.Pi*own()) +
			0.4*math.Sin((trembleSlowest+second)*turn+2*math.Pi*own())
	}
	dx = way()
	dy = way()
	return math.Round(trembleBy * dx), math.Round(trembleBy * dy)
}

// picture draws a grid of characters as it is at a moment: the cage's
// each in its cell, the name's each as far from its cell as it trembles.
// Each pixel is how much of it a character covers, from 0 to 255.
func picture(face *chargrid.Face, lines [][]byte, elapsed time.Duration) *image.Alpha {
	img := image.NewAlpha(image.Rect(0, 0, len(lines[0])*cellWidth, len(lines)*2*cellWidth))
	for row, line := range lines {
		for col, char := range line {
			if char == ' ' {
				continue
			}
			x, y := float64(col*cellWidth), float64(row*2*cellWidth)
			if col >= nameColumn {
				dx, dy := tremble(col, row, elapsed)
				x, y = x+dx, y+dy
			}
			face.Draw(img, image.Opaque, x, y, 2*cellWidth, rune(char))
		}
	}
	return img
}

// outside is whether a pixel is outside the Masthead's rounded corners.
func outside(x, y, width, height int) bool {
	dx := max(radius-x, x-(width-1-radius), 0)
	dy := max(radius-y, y-(height-1-radius), 0)
	return dx > 0 && dy > 0 && dx*dx+dy*dy > radius*radius
}

// masthead draws the Masthead, as the bytes of a GIF. The first picture
// is the whole of it; each one after it holds only the pixels that
// changed since the one before, in the smallest rectangle that has them
// all.
func masthead() ([]byte, error) {
	face, err := chargrid.NewFace(cellWidth)
	if err != nil {
		return nil, err
	}
	// The colors: nothing, for what is outside the corners and what did
	// not change, and then the shades from the ground to the characters.
	ground, ink := rgb(palette.Background), rgb(palette.Accent)
	colors := color.Palette{color.NRGBA{}}
	for i := 0; i < shades; i++ {
		mix := func(a, b uint8) uint8 { return uint8((int(a)*(shades-1-i) + int(b)*i) / (shades - 1)) }
		colors = append(colors, color.NRGBA{R: mix(ground.R, ink.R), G: mix(ground.G, ink.G), B: mix(ground.B, ink.B), A: 255})
	}
	const nothing = 0

	out := &gif.GIF{}
	var before []uint8
	for n := 0; n < frames; n++ {
		at := cage.Again * time.Duration(n) / frames
		covered := picture(face, grid(at), at)
		bounds := covered.Bounds()
		width, height := bounds.Dx(), bounds.Dy()
		now := make([]uint8, width*height)
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				if outside(x, y, width, height) {
					continue
				}
				now[y*width+x] = 1 + uint8((int(covered.AlphaAt(x, y).A)*(shades-1)+127)/255)
			}
		}
		// What changed, and the rectangle around it.
		changed := image.Rectangle{}
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				if before == nil || now[y*width+x] != before[y*width+x] {
					changed = changed.Union(image.Rect(x, y, x+1, y+1))
				}
			}
		}
		if changed.Empty() {
			changed = image.Rect(0, 0, 1, 1)
		}
		frame := image.NewPaletted(changed, colors)
		for y := changed.Min.Y; y < changed.Max.Y; y++ {
			for x := changed.Min.X; x < changed.Max.X; x++ {
				if i := y*width + x; before == nil || now[i] != before[i] {
					frame.SetColorIndex(x, y, now[i])
				} else {
					frame.SetColorIndex(x, y, nothing)
				}
			}
		}
		out.Image = append(out.Image, frame)
		out.Delay = append(out.Delay, delay)
		out.Disposal = append(out.Disposal, gif.DisposalNone)
		if n == 0 {
			out.Config = image.Config{ColorModel: colors, Width: width, Height: height}
		}
		before = now
	}
	var data bytes.Buffer
	if err := gif.EncodeAll(&data, out); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}
