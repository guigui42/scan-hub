package crop

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"testing"
)

// a4 returns a white A4 page at 100 dpi (827×1169).
func a4() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 827, 1169))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	return img
}

func fill(img *image.RGBA, r image.Rectangle, c color.Color) {
	draw.Draw(img, r, image.NewUniform(c), image.Point{}, draw.Src)
}

func near(t *testing.T, got Rect, want image.Rectangle, tol int) {
	t.Helper()
	g := got.Image()
	if abs(g.Min.X-want.Min.X) > tol || abs(g.Min.Y-want.Min.Y) > tol || abs(g.Max.X-want.Max.X) > tol || abs(g.Max.Y-want.Max.Y) > tol {
		t.Fatalf("crop %v, want %v ±%d", g, want, tol)
	}
}

func abs(v int) int { return max(v, -v) }

func TestDetectCard(t *testing.T) {
	img := a4()
	card := image.Rect(150, 200, 487, 413) // 85.6×54 mm at 100 dpi
	fill(img, card, color.RGBA{180, 200, 230, 255})
	fill(img, image.Rect(170, 220, 260, 330), color.RGBA{60, 50, 40, 255}) // photo
	r, ok := Detect(img, 100)
	if !ok {
		t.Fatal("no crop")
	}
	pad := 11 // padMM at 100 dpi
	near(t, r, card.Inset(-pad), 6)
}

// Real glass scans carry dust specks all over; they must not stretch the
// box (seen on an ET-2750 at 300 dpi).
func TestDetectIgnoresDust(t *testing.T) {
	img := a4()
	card := image.Rect(0, 0, 337, 213)
	fill(img, card, color.RGBA{150, 170, 200, 255})
	for _, p := range []image.Point{{650, 200}, {20, 650}, {300, 700}, {60, 900}, {700, 1100}, {400, 1150}, {25, 1100}, {26, 1120}} {
		fill(img, image.Rect(p.X, p.Y, p.X+3, p.Y+2), color.Black)
	}
	r, ok := Detect(img, 100)
	if !ok {
		t.Fatal("no crop")
	}
	if r.X+r.W > 360 || r.Y+r.H > 240 {
		t.Fatalf("crop %v stretched by dust", r)
	}
}

func TestDetectCardInCornerIgnoresGlassEdge(t *testing.T) {
	img := a4()
	card := image.Rect(0, 0, 337, 213)
	fill(img, card, color.RGBA{90, 90, 90, 255})
	fill(img, image.Rect(824, 0, 827, 1169), color.Black) // dark line at the glass edge
	r, ok := Detect(img, 100)
	if !ok {
		t.Fatal("no crop")
	}
	if r.X != 0 || r.Y != 0 {
		t.Fatalf("crop %v should start at the corner", r)
	}
	if r.X+r.W > 400 || r.Y+r.H > 260 {
		t.Fatalf("crop %v reaches past the card", r)
	}
}

// A thin rule far below the text must survive, even at 600 dpi where cells
// are 12 px apart.
func TestDetectKeepsThinContent(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 4960, 7016)) // A4 at 600 dpi
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(600, 600, 4000, 3000), image.Black, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(1000, 4201, 3000, 4203), image.Black, image.Point{}, draw.Src) // 2 px rule
	r, ok := Detect(img, 600)
	if !ok {
		t.Fatal("no crop")
	}
	if r.Y+r.H < 4203 {
		t.Fatalf("crop %v cuts the rule at y=4201", r)
	}
}

func TestDetectNothingToTrim(t *testing.T) {
	blank := a4()
	speck := a4()
	fill(speck, image.Rect(400, 600, 403, 603), color.Black)
	full := a4()
	fill(full, image.Rect(10, 10, 817, 1159), color.RGBA{120, 120, 120, 255})
	for name, img := range map[string]image.Image{"blank": blank, "speck": speck, "full-bleed": full} {
		if r, ok := Detect(img, 100); ok {
			t.Errorf("%s: unexpected crop %v", name, r)
		}
	}
}

func TestApplyAndValid(t *testing.T) {
	img := a4()
	got := Apply(img, Rect{10, 20, 30, 40}).Bounds()
	if got.Dx() != 30 || got.Dy() != 40 {
		t.Fatalf("Apply size %v", got)
	}
	for _, c := range []struct {
		r    Rect
		want bool
	}{
		{Rect{0, 0, 827, 1169}, true},
		{Rect{-1, 0, 100, 100}, false},
		{Rect{800, 0, 100, 100}, false},
		{Rect{0, 0, 8, 100}, false},
		{Rect{math.MaxInt - 10, 0, 100, 100}, false},
		{Rect{0, math.MaxInt - 10, 100, 100}, false},
	} {
		if c.r.Valid(827, 1169) != c.want {
			t.Errorf("%v.Valid = %v", c.r, !c.want)
		}
	}
}

// A lid that lets light in shades the glass from one corner, by more than
// the content threshold; the crop must keep to the document (seen on an
// ET-2750 with a passport).
func TestDetectIgnoresLidShading(t *testing.T) {
	img := a4()
	for y := range 1169 {
		for x := range 827 {
			d := max(0, 400-x-y) * 70 / 400 // darkest (180) at the top-left corner
			img.Set(x, y, color.RGBA{uint8(250 - d), uint8(250 - d), uint8(248 - d), 255})
		}
	}
	card := image.Rect(300, 400, 637, 613)
	fill(img, card, color.RGBA{180, 200, 230, 255})
	r, ok := Detect(img, 100)
	if !ok {
		t.Fatal("no crop")
	}
	near(t, r, card.Inset(-11), 6)
}

// A grey document running off the glass keeps all of it, faint marks
// included, though most of it is flat (no sharp marks nearby).
func TestDetectKeepsFlatDocumentAtTheEdge(t *testing.T) {
	img := a4()
	doc := image.Rect(0, 0, 600, 1169)
	fill(img, doc, color.Gray{210})
	fill(img, image.Rect(20, 500, 60, 503), color.Gray{190}) // faint pencil
	for i := range 8 {
		fill(img, image.Rect(100, 100+i*30, 500, 108+i*30), color.Black)
	}
	r, ok := Detect(img, 100)
	if !ok || r.X != 0 || r.X+r.W < 600 {
		t.Fatalf("crop %+v, %v; want the whole document, 0..611", r, ok)
	}
}
