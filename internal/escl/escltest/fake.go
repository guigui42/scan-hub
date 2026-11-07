// Package escltest is a fake eSCL flatbed scanner over TLS for tests.
package escltest

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/guigui42/scan-hub/internal/vens/venstest"
)

// Server is a running fake.
type Server struct {
	*httptest.Server

	NotReady int  // NextDocument answers 503 this many times first
	Busy     bool // ScanJobs answers 503
	Hang     bool // NextDocument blocks until the client goes away

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
		hang := s.Hang
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
		w.Write(venstest.PageJPEG(n, 0))
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
