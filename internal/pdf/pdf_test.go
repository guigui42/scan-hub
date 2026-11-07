package pdf

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/tiff"

	"github.com/guigui42/scan-hub/internal/vens/venstest"
)

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func grayJPEG(w, h int) []byte {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(i)
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

func bwTIFF(w, h int) []byte {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			if (x/10+y/10)%2 == 0 {
				img.SetGray(x, y, color.Gray{255})
			}
		}
	}
	var b bytes.Buffer
	tiff.Encode(&b, img, &tiff.Options{Compression: tiff.Deflate})
	return b.Bytes()
}

func TestMixedPages(t *testing.T) {
	dir := t.TempDir()
	colour := venstest.PageJPEG(0, 0) // 120x170
	pages := []Page{
		{Path: writeFile(t, dir, "a.jpg", colour), MIME: "image/jpeg", DPI: 300},
		{Path: writeFile(t, dir, "b.jpg", grayJPEG(300, 200)), MIME: "image/jpeg", DPI: 150}, // landscape
		{Path: writeFile(t, dir, "c.tif", bwTIFF(240, 340)), MIME: "image/tiff"},             // default 300 dpi
	}
	var out bytes.Buffer
	created := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if err := Write(&out, pages, Options{Title: "Facture été", Created: created}); err != nil {
		t.Fatal(err)
	}
	b := out.Bytes()
	if !bytes.HasPrefix(b, []byte("%PDF-")) || !bytes.Contains(b, []byte("%%EOF")) {
		t.Fatal("not a PDF")
	}
	if !bytes.Contains(b, []byte("/Count 3")) {
		t.Fatal("page count")
	}
	// 120x170 px @300 dpi = 28.80 x 40.80 pt; 300x200 @150 = 144 x 96 pt; 240x340 @300 = 57.60 x 81.60 pt.
	boxes := regexp.MustCompile(`/MediaBox \[0 0 ([0-9.]+) ([0-9.]+)\]`).FindAllStringSubmatch(string(b), -1)
	want := [][2]string{{"28.80", "40.80"}, {"144.00", "96.00"}, {"57.60", "81.60"}}
	// fpdf also writes its default (A4) box on the page-tree root, last.
	if len(boxes) != 4 {
		t.Fatalf("media boxes %v", boxes)
	}
	for i, w := range want {
		if boxes[i][1] != w[0] || boxes[i][2] != w[1] {
			t.Fatalf("page %d box %v, want %v", i+1, boxes[i][1:], w)
		}
	}
	if !bytes.Contains(b, colour) {
		t.Fatal("colour JPEG was re-encoded instead of passed through")
	}
	if !strings.Contains(string(b), "/Creator") || !bytes.Contains(b, []byte("D:20261107")) {
		t.Fatal("metadata missing")
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	if err := Write(&bytes.Buffer{}, nil, Options{}); err == nil {
		t.Fatal("empty accepted")
	}
	bad := writeFile(t, dir, "x.jpg", []byte("nope"))
	if err := Write(&bytes.Buffer{}, []Page{{Path: bad, MIME: "image/jpeg"}}, Options{}); err == nil {
		t.Fatal("corrupt JPEG accepted")
	}
	if err := Write(&bytes.Buffer{}, []Page{{Path: bad, MIME: "image/png"}}, Options{}); err == nil {
		t.Fatal("unsupported type accepted")
	}
}
