// Command appicon draws the app's icon: the cage of a zeolite's
// framework in ASCII art, drawn in characters of one flat color whose
// weights say how near each part of it is, on the palette's dark rounded
// square.
//
//	appicon <file.png>
package main

//go:generate go run . ../../resources/icon.png

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"

	"zeolite/internal/palette"
)

// cage is the art: a sodalite cage, the unit that the frameworks of
// sodalite, zeolite A and faujasite are built of, seen a little from the
// side of one of its six-membered rings. It is a truncated octahedron:
// twenty-four T-atoms, silicon or aluminium, each at a corner where two
// six-membered rings and one four-membered ring meet, and thirty-six
// edges, each an oxygen bridge between two of them. Nothing is drawn of
// its faces: a framework is its corners and its edges.
//
// A dot is nothing. Every other character is a mark, and says by its
// weight how near that part of the cage is: the ring that faces out is
// the heaviest strokes, the one behind it, seen through the cage, the
// lightest. A T-atom is a knot of the two heaviest marks: `@` for the
// twelve of the near half, `%` for the twelve of the far half, which no
// edge is drawn with.
const cage = `
.................................@@@@...................
................@@@xxxxxxxxxxxxxx@@@@...................
...............@@@@xxxxxxxxxxx....@@x**.................
.............**x@@@...............xxx*++................
...........****..XX................XXX.%%%%.............
........%%+**....XXX...............XXX!%%%!.............
.......%%%%......XXX...............;XXX..!!!............
.......+%%+!!!...XXX...........%;;;;@@@@..!!!...........
......+++....!!;%@@@@###############@@@@...!!!..........
......++.......%%@@@@@#############.@@@@#...!!!.........
.....++........;;@@@@...........::....####...!!!........
....+++........;###..............::....####...!!!.......
....++........;####...............::....####...!!%......
...++........;;###.................::....#XXX..!%%%.....
..+++........;###...................::....XXXX!!%%%.....
.%%+........;####....................::%.;;XXXX...++....
%%%%!!.....;;###......................%%%;..XXXX..++....
.%%.!!!!;;%%###.......................:%.....XX@@@.++...
.++......%%####......................::.......@@@@@++*..
.**........###......................::........@@@@@xx@@.
.**......@@@@;......................:.........XXX.xx@@@@
.***...XX@@@@;;....................::........XXX....@@@.
.***xXXXXX@@@#;;..................::........XXX....***..
@@@@xxX.....###:::................:.........XXX....**...
@@@@.........###.::.............%%%........XXX....**....
..xxxx........###.%%%:::::::::::%%%.......XXX....***....
....xxx........###%%%:...........::.......XXX...***.....
.....xxx........###;.............;;......XXX....**......
......xxx........###;.............;.....XXX....***......
.......xxx........####............;;..X@@@X...***.......
........xxx........#@@@@XXXXXXXXXXXXXX@@@@@xx@@@........
.........xxx......XX@@@@XXXXXXXXXXXXX..@@@+xx@@@........
..........x@@@xxxXXX@@@@.....!!!!!%%%%!+++..............
...........@@@@xx++%%%!!!!!!!!!!!!.%%...................
...........@@@.....%%%..................................
`

// nothing is the character of the art that draws nothing.
const nothing = '.'

// The icon's measures, in pixels of a 1024 pixel icon, as macOS draws
// its icons: a rounded square that leaves a margin, and room between the
// square's edge and the cage.
const (
	size   = 1024
	margin = 100
	radius = 185
	room   = 76
)

// The marks stand as text does across, and closer down: a line is as
// high as rowPitch of the font's size, so that the lines close up into
// one surface.
const (
	rowPitch = 0.86
	// characterWidth is the width of a character of Go Mono, as a part of
	// the font's size.
	characterWidth = 0.6
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: appicon <file.png>")
		os.Exit(2)
	}
	img, err := icon()
	if err == nil {
		var f *os.File
		if f, err = os.Create(os.Args[1]); err == nil {
			if err = png.Encode(f, img); err == nil {
				err = f.Close()
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "appicon:", err)
		os.Exit(1)
	}
}

// rgb reads a color of the palette.
func rgb(hex string) color.NRGBA {
	v, err := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	if err != nil || len(hex) != 7 {
		panic("appicon: " + hex + " is not a color of the palette")
	}
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}
}

// rows are the lines of the art.
func rows() []string {
	return strings.Split(strings.Trim(cage, "\n"), "\n")
}

// icon draws the icon.
func icon() (*image.NRGBA, error) {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	// The rounded square.
	var square vector.Rasterizer
	square.Reset(size, size)
	roundedRect(&square, margin, margin, size-margin, size-margin, radius)
	square.Draw(img, img.Bounds(), image.NewUniform(rgb(palette.Panel)), image.Point{})

	// The cage: the art is as wide as the square less the room around
	// it, and stands in the square's middle.
	lines := rows()
	columns := 0
	for _, l := range lines {
		columns = max(columns, len(l))
	}
	parsed, err := opentype.Parse(gomonobold.TTF)
	if err != nil {
		return nil, err
	}
	const side = size - 2*margin - 2*room
	cellW := float64(side) / float64(columns)
	points := cellW / characterWidth
	cellH := points * rowPitch
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: points, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	left := float64(margin + room)
	top := (size - float64(len(lines))*cellH) / 2
	metrics := face.Metrics()
	ascent := float64(metrics.Ascent) / 64
	glyph := float64(metrics.Ascent+metrics.Descent) / 64

	d := font.Drawer{Dst: img, Src: image.NewUniform(rgb(palette.Accent)), Face: face}
	for row, line := range lines {
		for col, r := range line {
			x, y := left+float64(col)*cellW, top+float64(row)*cellH
			if r == nothing {
				continue
			}
			d.Dot = fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6((y + (cellH-glyph)/2 + ascent) * 64)}
			d.DrawString(string(r))
		}
	}
	return img, nil
}

// roundedRect adds a rectangle with quarter-circle corners to a path.
func roundedRect(z *vector.Rasterizer, x0, y0, x1, y1, r float32) {
	// How far from a corner's ends the control points of a cubic curve
	// stand, for a quarter circle.
	const k = 0.5522847498
	z.MoveTo(x0+r, y0)
	z.LineTo(x1-r, y0)
	z.CubeTo(x1-r+k*r, y0, x1, y0+r-k*r, x1, y0+r)
	z.LineTo(x1, y1-r)
	z.CubeTo(x1, y1-r+k*r, x1-r+k*r, y1, x1-r, y1)
	z.LineTo(x0+r, y1)
	z.CubeTo(x0+r-k*r, y1, x0, y1-r+k*r, x0, y1-r)
	z.LineTo(x0, y0+r)
	z.CubeTo(x0, y0+r-k*r, x0+r-k*r, y0, x0+r, y0)
	z.ClosePath()
}
