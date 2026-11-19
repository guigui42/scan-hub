package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/tiff"

	"github.com/guigui42/scan-hub/internal/crop"
	"github.com/guigui42/scan-hub/internal/escl/escltest"
	"github.com/guigui42/scan-hub/internal/vens/venstest"
)

func open(t *testing.T, opt Options) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), opt)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateAddReload(t *testing.T) {
	s := open(t, Options{ThumbWidth: 60})
	m, err := s.Create("scansnap-button")
	if err != nil {
		t.Fatal(err)
	}
	img := venstest.PageJPEG(0, 1)
	p, err := s.AddPage(m.ID, img, "scansnap", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.MIME != "image/jpeg" || p.Width != 120 || p.Height != 170 || p.Size != int64(len(img)) {
		t.Fatalf("page %+v", p)
	}
	th, err := os.ReadFile(s.ThumbPath(m.ID, p))
	if err != nil {
		t.Fatal("no thumbnail:", err)
	}
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(th)); err != nil || cfg.Width != 60 || cfg.Height != 85 {
		t.Fatalf("thumb %+v %v", cfg, err)
	}
	if _, err := s.AddPage(m.ID, img, "scansnap", 1, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Pages) != 2 || got.Pages[0].File == got.Pages[1].File {
		t.Fatalf("manifest %+v", got)
	}
	b, err := os.ReadFile(s.PagePath(m.ID, got.Pages[0]))
	if err != nil || !bytes.Equal(b, img) {
		t.Fatal("page bytes differ")
	}
	sum := sha256.Sum256(b)
	if got.Pages[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("checksum mismatch")
	}
}

func TestTIFFPage(t *testing.T) {
	s := open(t, Options{})
	m, _ := s.Create("scansnap")
	var b bytes.Buffer
	tiff.Encode(&b, image.NewGray(image.Rect(0, 0, 50, 70)), nil)
	p, err := s.AddPage(m.ID, b.Bytes(), "scansnap", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.MIME != "image/tiff" || p.Width != 50 || p.Height != 70 || p.Thumb == "" {
		t.Fatalf("%+v", p)
	}
}

func TestDeletePageAndSession(t *testing.T) {
	s := open(t, Options{})
	m, _ := s.Create("epson")
	a, _ := s.AddPage(m.ID, venstest.PageJPEG(0, 0), "epson", 0, 0)
	b, _ := s.AddPage(m.ID, venstest.PageJPEG(1, 0), "epson", 0, 0)
	got, err := s.DeletePage(m.ID, a.File)
	if err != nil || len(got.Pages) != 1 || got.Pages[0].File != b.File {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := os.Stat(s.PagePath(m.ID, a)); !os.IsNotExist(err) {
		t.Fatal("page file left behind")
	}
	if _, err := os.Stat(s.ThumbPath(m.ID, a)); !os.IsNotExist(err) {
		t.Fatal("thumb left behind")
	}
	if _, err := s.DeletePage(m.ID, a.File); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete")
	}
	if _, err := s.DeletePage(m.ID, "../manifest.json"); !errors.Is(err, ErrNotFound) {
		t.Fatal("path accepted")
	}
	if err := s.Delete(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("session survived delete")
	}
}

func TestLimits(t *testing.T) {
	s := open(t, Options{MaxPages: 1})
	m, _ := s.Create("x")
	if _, err := s.AddPage(m.ID, venstest.PageJPEG(0, 0), "x", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPage(m.ID, venstest.PageJPEG(0, 0), "x", 0, 0); !errors.Is(err, ErrLimit) {
		t.Fatalf("page limit: %v", err)
	}

	s = open(t, Options{MaxSessionBytes: 100})
	m, _ = s.Create("x")
	if _, err := s.AddPage(m.ID, venstest.PageJPEG(0, 0), "x", 0, 0); !errors.Is(err, ErrLimit) {
		t.Fatalf("size limit: %v", err)
	}

	s = open(t, Options{})
	s.free = func(string) (int64, error) { return 100 << 20, nil }
	m, _ = s.Create("x")
	if _, err := s.AddPage(m.ID, venstest.PageJPEG(0, 0), "x", 0, 0); !errors.Is(err, ErrLimit) {
		t.Fatalf("disk limit: %v", err)
	}
	if got, _ := s.Get(m.ID); len(got.Pages) != 0 {
		t.Fatal("page stored despite limit")
	}
}

func TestListTitleCleanup(t *testing.T) {
	s := open(t, Options{})
	old, _ := s.Create("a")
	time.Sleep(10 * time.Millisecond)
	recent, _ := s.Create("b")
	if _, err := s.SetTitle(recent.ID, "Facture EDF"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	if len(list) != 2 || list[0].ID != recent.ID || list[0].Title != "Facture EDF" {
		t.Fatalf("list %+v", list)
	}
	gone, err := s.CleanupAbandoned(24*time.Hour, old.Updated.Add(24*time.Hour+5*time.Millisecond))
	if err != nil || len(gone) != 1 || gone[0] != old.ID {
		t.Fatalf("cleanup %v %v", gone, err)
	}
}

func TestRejectsUnknownFormatAndBadIDs(t *testing.T) {
	s := open(t, Options{})
	m, _ := s.Create("x")
	if _, err := s.AddPage(m.ID, []byte("not an image"), "x", 0, 0); err == nil {
		t.Fatal("accepted non-image")
	}
	for _, id := range []string{"../etc", "", "20261107-000000-zzzzzz"} {
		if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(%q) = %v", id, err)
		}
	}
}

func TestDPI(t *testing.T) {
	j := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 1, 0x01, 0x2C, 0x01, 0x2C}
	if d := jfifDPI(j); d != 300 {
		t.Fatalf("jfif dpi %d", d)
	}
	var b bytes.Buffer
	tiff.Encode(&b, image.NewGray(image.Rect(0, 0, 4, 4)), nil)
	// x/image/tiff writes 72 dpi.
	if d := tiffDPI(b.Bytes()); d != 72 {
		t.Fatalf("tiff dpi %d", d)
	}
}

func TestDeleteIfUnchanged(t *testing.T) {
	s := open(t, Options{})
	m, _ := s.Create("x")
	s.AddPage(m.ID, venstest.PageJPEG(0, 0), "x", 0, 0)
	snap, _ := s.Get(m.ID)
	time.Sleep(2 * time.Millisecond)
	s.AddPage(m.ID, venstest.PageJPEG(1, 0), "x", 0, 0) // added after the snapshot
	if err := s.DeleteIfUnchanged(m.ID, snap.Updated); !errors.Is(err, ErrChanged) {
		t.Fatalf("want ErrChanged, got %v", err)
	}
	cur, _ := s.Get(m.ID)
	if err := s.DeleteIfUnchanged(m.ID, cur.Updated); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("not deleted")
	}
}

func TestCrop(t *testing.T) {
	s := open(t, Options{ThumbWidth: 100})
	m, _ := s.Create("ui")
	img := escltest.GlassJPEG()
	p, err := s.AddPage(m.ID, img, "glass", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.DPI != 100 || p.Crop != nil || p.Rev() != "full" {
		t.Fatalf("page %+v", p)
	}

	got, ok, err := s.AutoCrop(m.ID, p.File)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	c := got.Pages[0].Crop
	// The card spans 24..361 × 24..237; 3 mm of padding is 11 px at 100 dpi.
	if c == nil || c.X != 13 || c.Y != 13 || c.X+c.W < 361 || c.X+c.W > 380 || c.Y+c.H < 237 || c.Y+c.H > 256 {
		t.Fatalf("crop %+v", c)
	}
	thumbSize := func(p Page) (int, int) {
		th, err := os.ReadFile(s.ThumbPath(m.ID, p))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(th))
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Width, cfg.Height
	}
	if w, h := thumbSize(got.Pages[0]); w != 100 || h != 100*c.H/c.W {
		t.Fatalf("thumb %dx%d for crop %+v", w, h, c)
	}
	if got.Pages[0].Thumb == p.Thumb {
		t.Fatal("a cropped rendering must get its own thumbnail file")
	}
	if _, err := os.Stat(s.ThumbPath(m.ID, p)); !os.IsNotExist(err) {
		t.Fatalf("old thumbnail left behind: %v", err)
	}
	if b, _ := os.ReadFile(s.PagePath(m.ID, got.Pages[0])); !bytes.Equal(b, img) {
		t.Fatal("original image altered")
	}

	if _, err := s.SetCrop(m.ID, p.File, &crop.Rect{X: 800, Y: 0, W: 100, H: 100}); !errors.Is(err, ErrBadCrop) {
		t.Fatalf("out-of-page crop: %v", err)
	}
	got, err = s.SetCrop(m.ID, p.File, &crop.Rect{X: 0, Y: 0, W: 827, H: 1169})
	if err != nil || got.Pages[0].Crop != nil {
		t.Fatalf("whole-page crop should clear: %+v %v", got.Pages[0].Crop, err)
	}
	if w, h := thumbSize(got.Pages[0]); w != 100 || h != 141 {
		t.Fatalf("thumb after reset %dx%d", w, h)
	}
	if _, err := s.SetCrop(m.ID, "nope.jpg", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown page: %v", err)
	}

	blank, _ := s.AddPage(m.ID, venstest.PageJPEG(0, 0), "glass", 0, 0)
	if _, ok, err := s.AutoCrop(m.ID, blank.File); ok || err != nil {
		t.Fatalf("full-bleed page cropped: %v %v", ok, err)
	}
}

func TestRotateAndStraighten(t *testing.T) {
	s := open(t, Options{ThumbWidth: 100})
	m, _ := s.Create("ui")
	img := escltest.TiltedGlassJPEG(3)
	p, err := s.AddPage(m.ID, img, "glass", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// A scan with auto-crop straightens the card, then trims around it.
	got, ok, err := s.AutoCrop(m.ID, p.File)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	q := got.Pages[0]
	if math.Abs(q.Angle+3) > 0.3 || q.Turns != 0 || q.Crop == nil {
		t.Fatalf("page %+v, want about -3° and a crop", q)
	}
	// 85.6×54 mm at 100 dpi plus 3 mm of padding each side.
	if q.Crop.W < 337 || q.Crop.W > 337+2*11+12 || q.Crop.H < 213 || q.Crop.H > 213+2*11+12 {
		t.Fatalf("crop %+v of the straightened card", q.Crop)
	}
	if q.Rev() == "full" || !strings.Contains(q.Rev(), "r0_") {
		t.Fatalf("rev %q ignores the rotation", q.Rev())
	}
	if b, _ := os.ReadFile(s.PagePath(m.ID, q)); !bytes.Equal(b, img) {
		t.Fatal("original image altered")
	}

	// A quarter turn makes it landscape; a crop is of the turned page.
	got, err = s.SetView(m.ID, p.File, crop.Rotation{Turns: 1}, nil)
	if err != nil || got.Pages[0].OutW() != 1169 || got.Pages[0].OutH() != 827 || got.Pages[0].Crop != nil {
		t.Fatalf("turned page %+v %v", got.Pages[0], err)
	}
	if _, err := s.SetCrop(m.ID, p.File, &crop.Rect{X: 900, Y: 0, W: 200, H: 800}); err != nil {
		t.Fatalf("crop inside the turned page: %v", err)
	}
	if _, err := s.SetView(m.ID, p.File, crop.Rotation{Angle: 60}, nil); !errors.Is(err, ErrBadCrop) {
		t.Fatalf("out-of-range angle: %v", err)
	}
	// Back to as scanned: the original thumbnail name again.
	got, err = s.SetView(m.ID, p.File, crop.Rotation{}, nil)
	if err != nil || got.Pages[0].Rev() != "full" || got.Pages[0].Thumb != p.Thumb {
		t.Fatalf("reset %+v %v", got.Pages[0], err)
	}

	// Straighten on a straight page: nothing to do.
	straight, _ := s.AddPage(m.ID, venstest.PageJPEG(0, 0), "glass", 0, 0)
	if _, ok, err := s.Detect(m.ID, straight.File, crop.Rotation{}, true); ok || err != nil {
		t.Fatalf("straight full-bleed page changed: %v %v", ok, err)
	}
}
