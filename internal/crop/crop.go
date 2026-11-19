// Package crop finds the document on a flatbed scan and cuts pages to a
// rectangle. Crops are non-destructive: callers keep the original image and
// store the rectangle next to it.
package crop

import (
	"image"
	"image/color"
	"image/draw"
	"math"
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
	workSize  = 600 // longest side of the sampling grid, in samples
	darkStep  = 28  // luminance drop below the background that counts as content
	chroma    = 24  // channel spread that counts as content (colour on white)
	edgeMM    = 1.5 // ignored border: glass edges often scan as a dark line
	padMM     = 3.0 // kept around the detected content
	minMM     = 10  // smaller content is dust
	dustMM    = 2.0 // marks spanning less than this (dust, specks) are ignored
	blobGapMM = 1.0 // marks closer than this belong together (letters, strokes)
	keepFrac  = 0.9 // a crop keeping more than this of the page isn't worth it
	localMM   = 3   // the background is judged within this distance
)

// Detect finds the content on a light background (the scanner lid) and
// returns the rectangle to keep, padded by a few millimetres. dpi converts
// millimetres to pixels (0 means 300). ok is false when there is nothing to
// trim: a blank page, or content covering most of it.
func Detect(img image.Image, dpi int) (r Rect, ok bool) {
	return detect(img, dpi, nil)
}

// DetectTurned is Detect on view, a w×h scan turned by r (see Rotate). It
// looks only at the scanned area: the corners the turn uncovered, and the
// border of the original scan, are neither content nor background.
func DetectTurned(view image.Image, dpi int, r Rotation, w, h int) (Rect, bool) {
	if dpi <= 0 {
		dpi = 300
	}
	if math.Abs(r.Angle) < minAngle {
		return detect(view, dpi, nil)
	}
	if r.Turns%2 == 1 {
		w, h = h, w
	}
	vb := view.Bounds()
	cx, cy := float64(vb.Dx())/2, float64(vb.Dy())/2
	rad := r.Angle * math.Pi / 180
	sin, cos := math.Sin(rad), math.Cos(rad)
	margin := edgeMM*float64(dpi)/25.4 + 1
	return detect(view, dpi, func(x, y float64) bool {
		// Back through the turn to the scan as it came off the glass.
		dx, dy := x-cx, y-cy
		sx, sy := cos*dx+sin*dy+float64(w)/2, -sin*dx+cos*dy+float64(h)/2
		return sx >= margin && sy >= margin && sx <= float64(w)-margin && sy <= float64(h)-margin
	})
}

// detect is Detect, minding only the pixels where scanned (nil: all).
func detect(img image.Image, dpi int, scanned func(x, y float64) bool) (r Rect, ok bool) {
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
	// A cell counts when all of it was scanned.
	in := make([]bool, gw*gh)
	var sorted []uint8
	for gy := range gh {
		for gx := range gw {
			x0, y0 := float64(gx*step), float64(gy*step)
			x1, y1 := float64(min(W, (gx+1)*step)), float64(min(H, (gy+1)*step))
			i := gy*gw + gx
			in[i] = scanned == nil || (scanned(x0, y0) && scanned(x1, y0) && scanned(x0, y1) && scanned(x1, y1))
			if in[i] {
				sorted = append(sorted, mean[i])
			}
		}
	}
	if len(sorted) == 0 {
		return Rect{}, false
	}
	// The background is the light end of the histogram, nearby: a lid
	// that let light in shades the glass smoothly over centimetres, while
	// a document's edges and print stand out from what's around them.
	slices.Sort(sorted)
	bg := int(sorted[len(sorted)*95/100])
	lit := make([]uint8, gw*gh)
	for i := range lit {
		if in[i] {
			lit[i] = mean[i]
		}
	}
	local := windowMax(lit, gw, gh, max(1, int(localMM*float64(dpi)/25.4)/step))
	content := func(i int) bool {
		return in[i] && (int(dark[i]) < min(bg, int(local[i]))-darkStep || spread[i] > chroma)
	}

	px := func(mm float64) int { return int(mm * float64(dpi) / 25.4) }
	edge := max(1, px(edgeMM)/step)
	if 2*edge >= gw || 2*edge >= gh {
		return Rect{}, false
	}
	x0, y0, x1, y1, found := blobs(gw, gh, edge, content, max(1, px(blobGapMM)/step), max(2, (px(dustMM)+step-1)/step))
	if !found {
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

// windowMax returns, for each cell of a w×h grid, the largest value within
// r cells either way.
func windowMax(v []uint8, w, h, r int) []uint8 {
	rows := make([]uint8, len(v))
	for y := range h {
		for x := range w {
			m := uint8(0)
			for k := max(0, x-r); k <= min(w-1, x+r); k++ {
				m = max(m, v[y*w+k])
			}
			rows[y*w+x] = m
		}
	}
	out := make([]uint8, len(v))
	for x := range w {
		for y := range h {
			m := uint8(0)
			for k := max(0, y-r); k <= min(h-1, y+r); k++ {
				m = max(m, rows[k*w+x])
			}
			out[y*w+x] = m
		}
	}
	return out
}

// blobs groups content cells (inside the ignored border) into clusters of
// marks no more than gap cells apart and returns the box around clusters
// spanning at least minSpan cells. Dust specks are tiny isolated clusters;
// text, pictures, thin rules and signatures join into larger ones.
func blobs(gw, gh, edge int, content func(int) bool, gap, minSpan int) (x0, y0, x1, y1 int, found bool) {
	seen := make([]bool, gw*gh)
	var stack []int
	for start := range seen {
		sx, sy := start%gw, start/gw
		if seen[start] || sx < edge || sx >= gw-edge || sy < edge || sy >= gh-edge || !content(start) {
			continue
		}
		bx0, by0, bx1, by1 := sx, sy, sx, sy
		seen[start] = true
		stack = append(stack[:0], start)
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			cx, cy := i%gw, i/gw
			bx0, by0, bx1, by1 = min(bx0, cx), min(by0, cy), max(bx1, cx), max(by1, cy)
			for ny := max(edge, cy-gap); ny <= min(gh-edge-1, cy+gap); ny++ {
				for nx := max(edge, cx-gap); nx <= min(gw-edge-1, cx+gap); nx++ {
					if j := ny*gw + nx; !seen[j] && content(j) {
						seen[j] = true
						stack = append(stack, j)
					}
				}
			}
		}
		if max(bx1-bx0, by1-by0)+1 < minSpan {
			continue
		}
		if !found {
			x0, y0, x1, y1, found = bx0, by0, bx1, by1, true
			continue
		}
		x0, y0, x1, y1 = min(x0, bx0), min(y0, by0), max(x1, bx1), max(y1, by1)
	}
	return x0, y0, x1, y1, found
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
