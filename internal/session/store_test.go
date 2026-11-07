package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"testing"
	"time"

	"golang.org/x/image/tiff"

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
