// Package pdf merges scanned pages into one PDF. JPEG pages are embedded
// unchanged (DCT passthrough); TIFF pages (ScanSnap B&W) become 1-bit PNGs.
// Each page is sized from its pixel dimensions and DPI.
package pdf

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // DecodeConfig
	"image/png"
	"io"
	"os"
	"time"

	"codeberg.org/go-pdf/fpdf"
	"golang.org/x/image/tiff"
)

// Page is one input image on disk.
type Page struct {
	Path string
	MIME string // image/jpeg or image/tiff
	DPI  int    // 0 = DefaultDPI
}

// DefaultDPI is used when a page carries no resolution.
const DefaultDPI = 300

// Options are document metadata.
type Options struct {
	Title   string
	Created time.Time // zero = now
}

// Write builds the PDF from pages, in order, into w.
func Write(w io.Writer, pages []Page, opt Options) error {
	if len(pages) == 0 {
		return errors.New("pdf: no pages")
	}
	if opt.Created.IsZero() {
		opt.Created = time.Now()
	}
	doc := fpdf.New("P", "mm", "A4", "")
	doc.SetAutoPageBreak(false, 0)
	doc.SetCreator("scan-hub", true)
	doc.SetProducer("scan-hub", true)
	doc.SetCreationDate(opt.Created)
	doc.SetModificationDate(opt.Created)
	if opt.Title != "" {
		doc.SetTitle(opt.Title, true)
	}

	for i, p := range pages {
		imgType, body, pw, ph, err := load(p)
		if err != nil {
			return fmt.Errorf("pdf: page %d: %w", i+1, err)
		}
		dpi := p.DPI
		if dpi <= 0 {
			dpi = DefaultDPI
		}
		wmm := float64(pw) / float64(dpi) * 25.4
		hmm := float64(ph) / float64(dpi) * 25.4
		// "P" keeps Wd/Ht as given; "L" would swap them.
		doc.AddPageFormat("P", fpdf.SizeType{Wd: wmm, Ht: hmm})
		name := fmt.Sprintf("p%d", i)
		doc.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: imgType}, body)
		doc.ImageOptions(name, 0, 0, wmm, hmm, false, fpdf.ImageOptions{}, 0, "")
		if doc.Err() {
			return fmt.Errorf("pdf: page %d: %w", i+1, doc.Error())
		}
	}
	return doc.Output(w)
}

func load(p Page) (imgType string, body io.Reader, w, h int, err error) {
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", nil, 0, 0, err
	}
	switch p.MIME {
	case "image/jpeg":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return "", nil, 0, 0, err
		}
		return "JPG", bytes.NewReader(data), cfg.Width, cfg.Height, nil
	case "image/tiff":
		img, err := tiff.Decode(bytes.NewReader(data))
		if err != nil {
			return "", nil, 0, 0, fmt.Errorf("decode TIFF: %w", err)
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, bitonal(img)); err != nil {
			return "", nil, 0, 0, err
		}
		b := img.Bounds()
		return "PNG", &buf, b.Dx(), b.Dy(), nil
	}
	return "", nil, 0, 0, fmt.Errorf("unsupported type %q", p.MIME)
}

var bw = color.Palette{color.Black, color.White}

// bitonal converts to a 2-colour paletted image (PNG writes it at 1 bit).
func bitonal(img image.Image) image.Image {
	if p, ok := img.(*image.Paletted); ok && len(p.Palette) <= 2 {
		return p
	}
	b := img.Bounds()
	out := image.NewPaletted(b, bw)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y >= 128 {
				out.SetColorIndex(x, y, 1)
			}
		}
	}
	return out
}
