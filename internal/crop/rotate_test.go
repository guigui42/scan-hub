package crop

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"testing"
)

// tiltedCard is a white A4 page at 100 dpi holding a card with a photo and
// text lines, turned by tilt degrees clockwise (as if laid askew on the
// glass).
func tiltedCard(tilt float64) image.Image {
	img := a4()
	fill(img, image.Rect(150, 200, 487, 413), color.RGBA{196, 214, 236, 255})
	fill(img, image.Rect(170, 230, 260, 360), color.RGBA{90, 78, 70, 255})
	for i := range 6 {
		fill(img, image.Rect(280, 240+i*22, 470-i*15, 249+i*22), color.RGBA{40, 50, 80, 255})
	}
	return Rotate(img, Rotation{Angle: tilt})
}

func TestSkewStraightensTiltedCard(t *testing.T) {
	for _, tilt := range []float64{2, -3.5, 0.6, 12} {
		got, ok := Skew(tiltedCard(tilt))
		if !ok || math.Abs(got+tilt) > 0.3 {
			t.Errorf("tilt %v°: Skew = %v, %v; want %v ±0.3", tilt, got, ok, -tilt)
		}
	}
}

func TestSkewLeavesStraightAndBlankPages(t *testing.T) {
	if got, ok := Skew(tiltedCard(0)); ok {
		t.Errorf("straight card: Skew = %v, want none", got)
	}
	if got, ok := Skew(a4()); ok {
		t.Errorf("blank page: Skew = %v, want none", got)
	}
}

func TestRotationSize(t *testing.T) {
	for _, c := range []struct {
		r    Rotation
		w, h int
	}{
		{Rotation{}, 800, 1000},
		{Rotation{Turns: 1}, 1000, 800},
		{Rotation{Turns: 2}, 800, 1000},
		{Rotation{Angle: 90}, 1000, 800},
		{Rotation{Angle: -30}, int(math.Round(800*math.Cos(math.Pi/6) + 1000*0.5)), int(math.Round(800*0.5 + 1000*math.Cos(math.Pi/6)))},
	} {
		if w, h := c.r.Size(800, 1000); w != c.w || h != c.h {
			t.Errorf("%+v: %dx%d, want %dx%d", c.r, w, h, c.w, c.h)
		}
	}
}

func TestRotateQuarterTurns(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	src.Set(0, 0, color.RGBA{255, 0, 0, 255}) // top-left marker
	for turns, want := range []image.Point{{0, 0}, {1, 0}, {2, 1}, {0, 2}} {
		out := Rotate(src, Rotation{Turns: turns})
		w, h := Rotation{Turns: turns}.Size(3, 2)
		if b := out.Bounds(); b.Dx() != w || b.Dy() != h {
			t.Fatalf("turns %d: %v, want %dx%d", turns, b, w, h)
		}
		if r, _, _, _ := out.At(want.X, want.Y).RGBA(); r>>8 != 255 {
			t.Errorf("turns %d: marker not at %v", turns, want)
		}
	}
	g := image.NewGray(image.Rect(0, 0, 4, 2))
	if _, ok := Rotate(g, Rotation{Turns: 1}).(*image.Gray); !ok {
		t.Error("a grey page should stay grey")
	}
}

func TestRotateAngleKeepsWholePage(t *testing.T) {
	img := a4()
	fill(img, image.Rect(0, 0, 827, 20), color.Black) // a dark top edge
	out := Rotate(img, Rotation{Angle: 10})
	w, h := Rotation{Angle: 10}.Size(827, 1169)
	if b := out.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("bounds %v, want %dx%d", b, w, h)
	}
	// Uncovered corners are white; the turned top edge stays inside.
	if r, g, b, _ := out.At(1, h-2).RGBA(); r>>8 < 250 || g>>8 < 250 || b>>8 < 250 {
		t.Error("corner not white")
	}
	// The middle of the top edge, turned about the centre, 10 px inwards.
	s, c := math.Sin(10*math.Pi/180), math.Cos(10*math.Pi/180)
	x, y := float64(w)/2+(1169/2-10)*s, float64(h)/2-(1169/2-10)*c
	if r, _, _, _ := out.At(int(x), int(y)).RGBA(); r>>8 > 60 {
		t.Error("top edge lost")
	}
}

// Glass scans have a grey lid; the white a turn uncovers must not make the
// lid look like content, or the crop keeps the whole tilted glass.
func TestDetectTurnedIgnoresUncoveredCorners(t *testing.T) {
	img := a4()
	fill(img, img.Bounds(), color.RGBA{215, 215, 212, 255}) // grey lid
	card := image.NewRGBA(image.Rect(0, 0, 337, 213))
	fill(card, card.Bounds(), color.RGBA{240, 236, 226, 255})
	fill(card, image.Rect(20, 40, 110, 170), color.RGBA{90, 78, 70, 255})
	for i := range 6 {
		fill(card, image.Rect(130, 50+i*20, 310-i*15, 58+i*20), color.RGBA{40, 50, 80, 255})
	}
	tilted := Rotate(card, Rotation{Angle: 4})
	draw.Draw(img, tilted.Bounds().Add(image.Pt(200, 300)), tilted, image.Point{}, draw.Src)

	r := Rotation{Angle: -4}
	view := Rotate(img, r)
	got, ok := DetectTurned(view, 100, r, 827, 1169)
	if !ok || got.W > 337+2*11+20 || got.H > 213+2*11+20 {
		t.Fatalf("crop %+v, %v; want at most the card, about 359x235", got, ok)
	}
	if plain, ok := Detect(view, 100); ok && plain.W < 600 {
		t.Fatalf("plain Detect %+v already copes: this test guards nothing", plain)
	}
}

func TestInscribedHoldsOnlyTheScan(t *testing.T) {
	for _, r := range []Rotation{{Angle: 2}, {Angle: -7.5}, {Turns: 1, Angle: 30}, {Turns: 3}} {
		w, h := 827, 1169
		in := Inscribed(r, w, h)
		vw, vh := r.Size(w, h)
		if !in.Valid(vw, vh) {
			t.Fatalf("%+v: %+v outside %dx%d", r, in, vw, vh)
		}
		tw, th := w, h
		if r.Turns%2 == 1 {
			tw, th = h, w
		}
		// Each corner, back through the fine turn, lies on the scan.
		s, c := math.Sin(r.Angle*math.Pi/180), math.Cos(r.Angle*math.Pi/180)
		for _, p := range [][2]int{{in.X, in.Y}, {in.X + in.W, in.Y}, {in.X, in.Y + in.H}, {in.X + in.W, in.Y + in.H}} {
			dx, dy := float64(p[0])-float64(vw)/2, float64(p[1])-float64(vh)/2
			sx, sy := c*dx+s*dy+float64(tw)/2, -s*dx+c*dy+float64(th)/2
			if sx < -0.5 || sy < -0.5 || sx > float64(tw)+0.5 || sy > float64(th)+0.5 {
				t.Errorf("%+v: corner %v maps off the scan to (%.1f, %.1f)", r, p, sx, sy)
			}
		}
		if r.Angle == 2 && in.W < 760 {
			t.Errorf("%+v: %+v needlessly small", r, in)
		}
	}
}

// Lid shading alone gives nothing to straighten by.
func TestSkewIgnoresLidShading(t *testing.T) {
	img := a4()
	for y := range 1169 {
		for x := range 827 {
			d := uint8(max(0, 400-x-y) * 70 / 400)
			img.Set(x, y, color.Gray{250 - d})
		}
	}
	if got, ok := Skew(img); ok {
		t.Errorf("shading: Skew = %v, want none", got)
	}
}
