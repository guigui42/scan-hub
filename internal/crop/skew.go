package crop

import (
	"image"
	"math"
	"slices"
)

const (
	skewSize  = 1000 // longest side of the sampling grid, in samples
	skewRange = 15.0 // searched angles, in degrees either way
	skewMin   = 0.1  // smaller tilts aren't worth resampling for
	skewEdges = 50   // fewer content edges than this: nothing to go by
	skewGain  = 1.02 // the best angle must beat "as scanned" by this much
)

// Skew estimates how far the content of a page is tilted and returns the
// clockwise angle in degrees that straightens it (see Rotation.Angle). ok
// is false when the page looks straight, blank or gives nothing to go by.
//
// It looks at the edges of the content (text lines, the outline of a card
// or a passport page) and picks the angle at which they line up best into
// horizontal rows: the projection profile method.
func Skew(img image.Image) (angle float64, ok bool) {
	b := img.Bounds()
	W, H := b.Dx(), b.Dy()
	if W < MinSize || H < MinSize {
		return 0, false
	}
	step := max(1, (max(W, H)+skewSize-1)/skewSize)
	gw, gh := (W+step-1)/step, (H+step-1)/step
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
	sorted := slices.Clone(mean)
	slices.Sort(sorted)
	bg := int(sorted[len(sorted)*95/100])
	// Marks stand out from what's within a few millimetres (a hundredth
	// of the page); lid shading doesn't (see Detect).
	local := windowMax(mean, gw, gh, max(1, max(gw, gh)/100))
	content := func(gx, gy int) bool {
		i := gy*gw + gx
		return int(dark[i]) < min(bg, int(local[i]))-darkStep || spread[i] > chroma
	}

	// Edge cells: content next to background, away from the glass border.
	edge := 2
	var xs, ys []float64
	cx, cy := float64(gw)/2, float64(gh)/2
	for gy := edge; gy < gh-edge; gy++ {
		for gx := edge; gx < gw-edge; gx++ {
			if content(gx, gy) && (!content(gx-1, gy) || !content(gx+1, gy) || !content(gx, gy-1) || !content(gx, gy+1)) {
				xs = append(xs, float64(gx)-cx)
				ys = append(ys, float64(gy)-cy)
			}
		}
	}
	if len(xs) < skewEdges {
		return 0, false
	}

	span := int(math.Hypot(float64(gw), float64(gh))) + 2
	hist := make([]float64, span)
	score := func(deg float64) float64 {
		clear(hist)
		s, c := math.Sin(deg*math.Pi/180), math.Cos(deg*math.Pi/180)
		for i := range xs {
			// Row of the edge once turned clockwise by deg.
			hist[int(xs[i]*s+ys[i]*c+float64(span)/2)]++
		}
		var t float64
		for _, h := range hist {
			t += h * h
		}
		return t
	}
	best, top := 0.0, score(0)
	base := top
	search := func(from, to, by float64) {
		for a := from; a <= to+by/2; a += by {
			if v := score(a); v > top {
				best, top = a, v
			}
		}
	}
	search(-skewRange, skewRange, 0.5)
	// Best at the end of the range: the true tilt is likely past it, or
	// there is none.
	if math.Abs(best) >= skewRange {
		return 0, false
	}
	search(max(-skewRange, best-0.5), min(skewRange, best+0.5), 0.05)
	best = math.Round(best*100) / 100
	if math.Abs(best) < skewMin || top < skewGain*base {
		return 0, false
	}
	return best, true
}
