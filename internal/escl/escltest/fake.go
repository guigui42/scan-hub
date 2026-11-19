// Package escltest is a fake eSCL flatbed scanner over TLS for tests.
package escltest

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/guigui42/scan-hub/internal/crop"
)

// Server is a running fake.
type Server struct {
	*httptest.Server

	NotReady int     // NextDocument answers 503 this many times first
	Busy     bool    // ScanJobs answers 503
	Hang     bool    // NextDocument blocks until the client goes away
	Tilt     float64 // degrees the card lies askew on the glass, clockwise

	mu       sync.Mutex
	nextID   int
	jobs     map[string]int // job id -> documents served
	Settings []string       // posted ScanSettings bodies
	Deleted  []string
	notReady atomic.Int32
}

// Start launches the fake with a self-signed certificate.
func Start() *Server {
	s := &Server{jobs: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /eSCL/ScannerCapabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, Capabilities)
	})
	mux.HandleFunc("GET /eSCL/ScannerStatus", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, `<?xml version="1.0"?><scan:ScannerStatus xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm"><pwg:Version>2.63</pwg:Version><pwg:State>Idle</pwg:State></scan:ScannerStatus>`)
	})
	mux.HandleFunc("POST /eSCL/ScanJobs", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.Busy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.Settings = append(s.Settings, string(b))
		s.nextID++
		id := fmt.Sprintf("job%d", s.nextID)
		s.jobs[id] = 0
		s.notReady.Store(int32(s.NotReady))
		// Like real devices: an absolute URL with another scheme/host.
		w.Header().Set("Location", "http://printer.local:80/eSCL/ScanJobs/"+id)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /eSCL/ScanJobs/{id}/NextDocument", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		s.mu.Lock()
		n, ok := s.jobs[id]
		hang, tilt := s.Hang, s.Tilt
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if hang {
			<-r.Context().Done()
			return
		}
		if s.notReady.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if n > 0 {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.jobs[id] = n + 1
		s.mu.Unlock()
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(TiltedGlassJPEG(tilt))
	})
	mux.HandleFunc("DELETE /eSCL/ScanJobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.Deleted = append(s.Deleted, r.PathValue("id"))
		delete(s.jobs, r.PathValue("id"))
		s.mu.Unlock()
	})
	s.Server = httptest.NewTLSServer(mux)
	return s
}

// GlassJPEG is an A4 scan at 100 dpi (827×1169, JFIF density set) of an
// ID card lying in the top-left corner of an otherwise empty glass.
func GlassJPEG() []byte { return TiltedGlassJPEG(0) }

// TiltedGlassJPEG is GlassJPEG with the card turned by tilt degrees
// clockwise, as if laid askew on the glass.
func TiltedGlassJPEG(tilt float64) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 827, 1169))
	glass := color.RGBA{250, 250, 248, 255}
	draw.Draw(img, img.Bounds(), image.NewUniform(glass), image.Point{}, draw.Src)
	card := image.NewRGBA(image.Rect(0, 0, 337, 213)) // 85.6×54 mm
	fill := func(r image.Rectangle, c color.Color) {
		draw.Draw(card, r, image.NewUniform(c), image.Point{}, draw.Src)
	}
	fill(card.Bounds(), color.RGBA{196, 214, 236, 255})
	fill(image.Rect(16, 46, 106, 176), color.RGBA{90, 78, 70, 255}) // photo
	for i := range 6 {
		fill(image.Rect(126, 56+i*20, 306-i*15, 64+i*20), color.RGBA{40, 50, 80, 255}) // text
	}
	turned := crop.Rotate(card, crop.Rotation{Angle: tilt})
	draw.Draw(img, turned.Bounds().Add(image.Pt(24, 24)), turned, image.Point{}, draw.Src)
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 85})
	out := b.Bytes()
	// SOI, then a JFIF APP0 segment: version 1.1, 100×100 dots per inch.
	app0 := []byte{0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 1, 0, 100, 0, 100, 0, 0}
	return append(append(out[:2:2], app0...), out[2:]...)
}

// SetTilt sets Tilt while the server runs.
func (s *Server) SetTilt(deg float64) {
	s.mu.Lock()
	s.Tilt = deg
	s.mu.Unlock()
}

// LastSettings returns the most recent ScanSettings body.
func (s *Server) LastSettings() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Settings) == 0 {
		return ""
	}
	return s.Settings[len(s.Settings)-1]
}

// DeletedJobs returns the deleted job IDs.
func (s *Server) DeletedJobs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Deleted...)
}

// Capabilities is the ET-2750's ScannerCapabilities (serial removed).
var Capabilities = strings.TrimSpace(capabilitiesXML)
