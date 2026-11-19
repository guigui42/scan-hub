package crop

import (
	"image"
	"image/color"
	"math"

	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
)

// Rotation turns a page upright: Turns quarter turns clockwise, then Angle
// degrees clockwise (the fine straightening). The zero value leaves the page
// as scanned.
type Rotation struct {
	Turns int     `json:"turns,omitempty"`
	Angle float64 `json:"angle,omitempty"`
}

// MaxAngle bounds the fine angle in degrees: past it, quarter turns apply.
const MaxAngle = 45

// minAngle is the smallest fine angle worth resampling a page for.
const minAngle = 0.01

// Valid reports whether r is in range: 0–3 turns, |Angle| ≤ MaxAngle.
func (r Rotation) Valid() bool {
	return r.Turns >= 0 && r.Turns <= 3 && !math.IsNaN(r.Angle) && math.Abs(r.Angle) <= MaxAngle
}

// IsZero reports whether r leaves the page as scanned.
func (r Rotation) IsZero() bool { return r.Turns == 0 && math.Abs(r.Angle) < minAngle }

// Size returns the size of a w×h page after r: quarter turns swap the
// sides, and a fine angle grows the page to the box around the turned one.
func (r Rotation) Size(w, h int) (int, int) {
	if r.Turns%2 == 1 {
		w, h = h, w
	}
	if math.Abs(r.Angle) < minAngle {
		return w, h
	}
	s, c := math.Abs(math.Sin(r.Angle*math.Pi/180)), math.Abs(math.Cos(r.Angle*math.Pi/180))
	fw, fh := float64(w), float64(h)
	return int(math.Round(fw*c + fh*s)), int(math.Round(fw*s + fh*c))
}

// Rotate returns img turned by r, with white where the turned page leaves
// corners uncovered. Quarter turns are exact; a fine angle is resampled
// bilinearly.
func Rotate(img image.Image, r Rotation) image.Image {
	if r.IsZero() {
		return img
	}
	if r.Turns%4 != 0 {
		img = turn(img, r.Turns%4)
	}
	if math.Abs(r.Angle) < minAngle {
		return img
	}
	b := img.Bounds()
	w, h := Rotation{Angle: r.Angle}.Size(b.Dx(), b.Dy())
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	// Source to destination: move the source centre to the origin, turn
	// (clockwise on screen, as y points down), then move to the new centre.
	rad := r.Angle * math.Pi / 180
	sin, cos := math.Sin(rad), math.Cos(rad)
	sx, sy := float64(b.Min.X)+float64(b.Dx())/2, float64(b.Min.Y)+float64(b.Dy())/2
	dx, dy := float64(w)/2, float64(h)/2
	m := f64.Aff3{
		cos, -sin, dx - cos*sx + sin*sy,
		sin, cos, dy - sin*sx - cos*sy,
	}
	draw.BiLinear.Transform(dst, m, img, b, draw.Over, nil)
	return dst
}

// View is a page as it goes into the PDF: rotated, then cropped (c is in
// pixels of the rotated page; nil keeps all of it).
func View(img image.Image, r Rotation, c *Rect) image.Image {
	img = Rotate(img, r)
	if c != nil {
		img = Apply(img, *c)
	}
	return img
}

// turn rotates img by t quarter turns clockwise, keeping grey pages grey.
func turn(img image.Image, t int) image.Image {
	b := img.Bounds()
	W, H := b.Dx(), b.Dy()
	w, h := W, H
	if t%2 == 1 {
		w, h = H, W
	}
	to := func(x, y int) (int, int) {
		switch t {
		case 1:
			return H - 1 - y, x
		case 2:
			return W - 1 - x, H - 1 - y
		default:
			return y, W - 1 - x
		}
	}
	switch img.(type) {
	case *image.Gray, *image.Paletted:
		out := image.NewGray(image.Rect(0, 0, w, h))
		eachPixel(img, func(x, y int, r, g, bl uint8) {
			nx, ny := to(x, y)
			out.Pix[ny*out.Stride+nx] = color.GrayModel.Convert(color.RGBA{r, g, bl, 255}).(color.Gray).Y
		})
		return out
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	eachPixel(img, func(x, y int, r, g, bl uint8) {
		nx, ny := to(x, y)
		i := ny*out.Stride + nx*4
		out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = r, g, bl, 255
	})
	return out
}
