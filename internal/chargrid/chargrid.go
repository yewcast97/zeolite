// Package chargrid draws characters into an image as the app's pictures
// have them: in Go Mono Bold, each in a cell of a grid. The icon and the
// Masthead are drawn with it.
package chargrid

import (
	"image"
	"image/draw"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// advance is the width of a character of Go Mono, as a part of the
// font's size.
const advance = 0.6

// Face is the font at one size.
type Face struct {
	face font.Face
	// Size is the font's size, in pixels.
	Size float64
	// ascent is how far the font's characters rise over their baseline,
	// and height how high a line of them is from its top to its bottom.
	ascent, height float64
}

// NewFace is the font at the size at which a character is width wide,
// in pixels.
func NewFace(width float64) (*Face, error) {
	parsed, err := opentype.Parse(gomonobold.TTF)
	if err != nil {
		return nil, err
	}
	size := width / advance
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	metrics := face.Metrics()
	return &Face{face: face, Size: size, ascent: float64(metrics.Ascent) / 64, height: float64(metrics.Ascent+metrics.Descent) / 64}, nil
}

// Draw draws a character in a color over an image: at x from its left,
// and in the middle of the line whose top is y and which is line high.
func (f *Face) Draw(dst draw.Image, src image.Image, x, y, line float64, char rune) {
	d := font.Drawer{Dst: dst, Src: src, Face: f.face}
	d.Dot = fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6((y + (line-f.height)/2 + f.ascent) * 64)}
	d.DrawString(string(char))
}
