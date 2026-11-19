// Package crop finds the document on a flatbed scan and cuts pages to a
// rectangle. Crops are non-destructive: callers keep the original image and
// store the rectangle next to it.
package crop

import (
	"image"
	"image/color"
	"image/draw"
	"slices"
)

// Rect is a crop in pixels of the original image.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Image returns r as an image.Rectangle.
func (r Rect) Image() image.Rectangle { return image.Rect(r.X, r.Y, r.X+r.W, r.Y+r.H) }

// MinSize is the smallest crop side in pixels.
const MinSize = 16

// Valid reports whether r lies inside a w×h image and is at least MinSize
// on each side.
func (r Rect) Valid(w, h int) bool {
	// Written so that no sum can overflow.
	return r.X >= 0 && r.Y >= 0 && r.W >= MinSize && r.H >= MinSize && r.X <= w && r.Y <= h && r.W <= w-r.X && r.H <= h-r.Y
}

// Full reports whether r covers the whole w×h image.
func (r Rect) Full(w, h int) bool { return r == Rect{0, 0, w, h} }

const (
	workSize = 600 // longest side of the sampling grid, in samples
	darkStep = 28  // luminance drop below the background that counts as content
	chroma   = 24  // channel spread that counts as content (colour on white)
	edgeMM   = 1.5 // ignored border: glass edges often scan as a dark line
	padMM    = 3.0 // kept around the detected content
	minMM    = 10  // smaller content is dust
	keepFrac = 0.9 // a crop keeping more than this of the page isn't worth it
)

// Detect finds the content on a light background (the scanner lid) and
// returns the rectangle to keep, padded by a few millimetres. dpi converts
// millimetres to pixels (0 means 300). ok is false when there is nothing to
// trim: a blank page, or content covering most of it.
func Detect(img image.Image, dpi int) (r Rect, ok bool) {
	if dpi <= 0 {
		dpi = 300
	}
	b := img.Bounds()
	W, H := b.Dx(), b.Dy()
	if W < MinSize || H < MinSize {
		return Rect{}, false
	}
	step := max(1, (max(W, H)+workSize-1)/workSize)
	gw, gh := (W+step-1)/step, (H+step-1)/step

	// Each cell pools every pixel of its step×step block: its mean
	// luminance (for the background level), darkest luminance and widest
	// colour spread (so a one-pixel line or pen stroke is never missed).
	sum := make([]int, gw*gh)
	dark := make([]uint8, gw*gh)
	spread := make([]uint8, gw*gh)
	for i := range dark {
		dark[i] = 255
	}
	eachPixel(img, func(x, y int, r8, g8, b8 uint8) {
		i := (y/step)*gw + x/step
		l := uint8((299*int(r8) + 587*int(g8) + 114*int(b8)) / 1000)
		sum[i] += int(l)
		dark[i] = min(dark[i], l)
		spread[i] = max(spread[i], max(r8, g8, b8)-min(r8, g8, b8))
	})
	mean := make([]uint8, gw*gh)
	for gy := range gh {
		for gx := range gw {
			n := (min(W, (gx+1)*step) - gx*step) * (min(H, (gy+1)*step) - gy*step)
			mean[gy*gw+gx] = uint8(sum[gy*gw+gx] / n)
		}
	}
	// The background is the light end of the histogram.
	sorted := slices.Clone(mean)
	slices.Sort(sorted)
	bg := int(sorted[len(sorted)*95/100])
	content := func(i int) bool { return int(dark[i]) < bg-darkStep || spread[i] > chroma }

	px := func(mm float64) int { return int(mm * float64(dpi) / 25.4) }
	edge := max(1, px(edgeMM)/step)
	if 2*edge >= gw || 2*edge >= gh {
		return Rect{}, false
	}
	// A row or column holds content when enough of its samples do, so
	// specks of dust don't stretch the box.
	rowMin, colMin := max(2, gw/200), max(2, gh/200)

	y0, y1 := -1, -1
	for gy := edge; gy < gh-edge; gy++ {
		n := 0
		for gx := edge; gx < gw-edge; gx++ {
			if content(gy*gw + gx) {
				n++
			}
		}
		if n >= rowMin {
			if y0 < 0 {
				y0 = gy
			}
			y1 = gy
		}
	}
	if y0 < 0 {
		return Rect{}, false
	}
	x0, x1 := -1, -1
	for gx := edge; gx < gw-edge; gx++ {
		n := 0
		for gy := y0; gy <= y1; gy++ {
			if content(gy*gw + gx) {
				n++
			}
		}
		if n >= colMin {
			if x0 < 0 {
				x0 = gx
			}
			x1 = gx
		}
	}
	if x0 < 0 {
		return Rect{}, false
	}
	// Content running into the ignored border reaches the edge.
	if x0 == edge {
		x0 = 0
	}
	if y0 == edge {
		y0 = 0
	}
	if x1 == gw-edge-1 {
		x1 = gw - 1
	}
	if y1 == gh-edge-1 {
		y1 = gh - 1
	}

	pad := px(padMM)
	left, top := max(0, x0*step-pad), max(0, y0*step-pad)
	right, bottom := min(W, (x1+1)*step+pad), min(H, (y1+1)*step+pad)
	r = Rect{X: left, Y: top, W: right - left, H: bottom - top}
	if r.W < px(minMM) || r.H < px(minMM) || !r.Valid(W, H) {
		return Rect{}, false
	}
	if float64(r.W*r.H) >= keepFrac*float64(W*H) {
		return Rect{}, false
	}
	return r, true
}

// eachPixel calls fn with every pixel's 8-bit RGB, relative to the image
// origin, with fast paths for what scanners decode to (JPEG: YCbCr or Gray;
// TIFF: Gray or Paletted).
func eachPixel(img image.Image, fn func(x, y int, r, g, b uint8)) {
	b := img.Bounds()
	switch m := img.(type) {
	case *image.YCbCr:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				yi, ci := m.YOffset(x, y), m.COffset(x, y)
				r, g, bl := color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci])
				fn(x-b.Min.X, y-b.Min.Y, r, g, bl)
			}
		}
	case *image.Gray:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				v := m.Pix[m.PixOffset(x, y)]
				fn(x-b.Min.X, y-b.Min.Y, v, v, v)
			}
		}
	default:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl, _ := img.At(x, y).RGBA()
				fn(x-b.Min.X, y-b.Min.Y, uint8(r>>8), uint8(g>>8), uint8(bl>>8))
			}
		}
	}
}

// Apply returns the part of img inside r (relative to img's origin).
func Apply(img image.Image, r Rect) image.Image {
	b := img.Bounds()
	rect := r.Image().Add(b.Min).Intersect(b)
	if s, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return s.SubImage(rect)
	}
	out := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(out, out.Bounds(), img, rect.Min, draw.Src)
	return out
}
