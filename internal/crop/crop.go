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
	// The background is the light end of the histogram.
	slices.Sort(sorted)
	bg := int(sorted[len(sorted)*95/100])
	content := func(i int) bool {
		return in[i] && (int(dark[i]) < bg-darkStep || spread[i] > chroma)
	}
	// A lid that lets light in shades the glass smoothly over centimetres,
	// darker than the background yet without a single sharp mark: a
	// region of content only counts when it holds marks that stand out
	// from what's within a few millimetres (print, a document's edge).
	lit := make([]uint8, gw*gh)
	for i := range lit {
		if in[i] {
			lit[i] = mean[i]
		}
	}
	local := windowMax(lit, gw, gh, max(1, int(localMM*float64(dpi)/25.4)/step))
	sharp := func(i int) bool {
		return in[i] && (int(dark[i]) < min(bg, int(local[i]))-darkStep || spread[i] > chroma)
	}

	px := func(mm float64) int { return int(mm * float64(dpi) / 25.4) }
	edge := max(1, px(edgeMM)/step)
	if 2*edge >= gw || 2*edge >= gh {
		return Rect{}, false
	}
	gap, minSpan := max(1, px(blobGapMM)/step), max(2, (px(dustMM)+step-1)/step)
	regions, regionBox := label(gw, gh, edge, content, gap)
	marks, markBox := label(gw, gh, edge, sharp, gap)
	// A region is a document when it holds marks and its outline against
	// the background is mostly crisp; otherwise (shading, or shading run
	// into a document) only its marks count.
	marked := make([]bool, len(regionBox))
	outline, crisp := make([]int, len(regionBox)), make([]int, len(regionBox))
	var keepBox []box
	for _, b := range markBox {
		if b.span() >= minSpan {
			keepBox = append(keepBox, b)
		}
	}
	for i, k := range regions {
		if k < 0 {
			continue
		}
		if m := marks[i]; m >= 0 && markBox[m].span() >= minSpan {
			marked[k] = true
		}
		x, y := i%gw, i/gw
		for _, d := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			nx, ny := x+d[0], y+d[1]
			if nx < edge || ny < edge || nx >= gw-edge || ny >= gh-edge {
				continue
			}
			if j := ny*gw + nx; in[j] && regions[j] < 0 {
				outline[k]++
				// By the mean: a cell's darkest pixel is noisy.
				if int(mean[i]) < int(local[i])-darkStep || spread[i] > chroma {
					crisp[k]++
				}
				break
			}
		}
	}
	for k, b := range regionBox {
		if marked[k] && 2*crisp[k] >= outline[k] {
			keepBox = append(keepBox, b)
		}
	}
	var x0, y0, x1, y1 int
	found := false
	for _, b := range keepBox {
		if !found {
			x0, y0, x1, y1, found = b.x0, b.y0, b.x1, b.y1, true
			continue
		}
		x0, y0, x1, y1 = min(x0, b.x0), min(y0, b.y0), max(x1, b.x1), max(y1, b.y1)
	}
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

type box struct{ x0, y0, x1, y1 int }

func (b box) span() int { return max(b.x1-b.x0, b.y1-b.y0) + 1 }

// label groups the content cells inside the ignored border into clusters
// of marks no more than gap cells apart (letters, strokes and lines join;
// dust specks stay small). It returns each cell's cluster (-1 for none)
// and each cluster's box.
func label(gw, gh, edge int, content func(int) bool, gap int) ([]int, []box) {
	labels := make([]int, gw*gh)
	for i := range labels {
		labels[i] = -1
	}
	var boxes []box
	var stack []int
	for start := range labels {
		sx, sy := start%gw, start/gw
		if labels[start] >= 0 || sx < edge || sx >= gw-edge || sy < edge || sy >= gh-edge || !content(start) {
			continue
		}
		n := len(boxes)
		b := box{sx, sy, sx, sy}
		labels[start] = n
		stack = append(stack[:0], start)
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			cx, cy := i%gw, i/gw
			b = box{min(b.x0, cx), min(b.y0, cy), max(b.x1, cx), max(b.y1, cy)}
			for ny := max(edge, cy-gap); ny <= min(gh-edge-1, cy+gap); ny++ {
				for nx := max(edge, cx-gap); nx <= min(gw-edge-1, cx+gap); nx++ {
					if j := ny*gw + nx; labels[j] < 0 && content(j) {
						labels[j] = n
						stack = append(stack, j)
					}
				}
			}
		}
		boxes = append(boxes, b)
	}
	return labels, boxes
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
