// Package paperlesstest is a fake Paperless-ngx 3.3 (API v10) for tests.
package paperlesstest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Outcome decides how a consumed upload ends.
type Outcome int

const (
	Succeed   Outcome = iota // result_data.document_id
	Duplicate                // status failure + result_data.duplicate_of (as 3.3 does)
	Fail                     // status failure + error_message
	Stopped                  // status success + reason, no document
)

// Upload is what the fake received.
type Upload struct {
	Filename string
	Title    string
	Tags     []string
	Body     []byte
	TaskID   string
}

type task struct {
	id       string
	filename string
	created  time.Time
	polls    int
	title    string
	outcome  Outcome
	docID    int
}

// Server is a running fake.
type Server struct {
	*httptest.Server
	Token string

	mu          sync.Mutex
	uploads     []Upload
	tasks       map[string]*task
	nextDoc     int
	Next        Outcome // outcome of the next upload
	PendingFor  int     // polls answered "started" before the outcome
	FailUploads int     // next N uploads answer 503 without consuming
	DropAfter   int     // next N uploads are consumed, then the connection is cut
	// ForbidDocuments answers 403 on /api/documents/, like a token without
	// view_document.
	ForbidDocuments bool
	// FailDocumentsAfter makes /api/documents/ answer 500 after this many
	// successful requests (0: never).
	FailDocumentsAfter int
	docRequests        int
	docs               map[int]string // document id -> title
}

// Start launches the fake (plain HTTP).
func Start(token string) *Server {
	s := &Server{Token: token, tasks: map[string]*task{}, nextDoc: 100, docs: map[int]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/documents/post_document/", s.postDocument)
	mux.HandleFunc("GET /api/tasks/", s.listTasks)
	mux.HandleFunc("GET /api/documents/", s.listDocuments)
	s.Server = httptest.NewServer(s.auth(mux))
	return s
}

func (s *Server) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token "+s.Token {
			http.Error(w, `{"detail":"Invalid token."}`, http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "version=10") {
			http.Error(w, `{"detail":"version"}`, http.StatusNotAcceptable)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) postDocument(w http.ResponseWriter, r *http.Request) {
	// Like Django, a body without Content-Length (chunked) reads as empty.
	if r.ContentLength <= 0 {
		http.Error(w, `{"document":["No file was submitted."]}`, http.StatusBadRequest)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, fh, err := r.FormFile("document")
	if err != nil {
		http.Error(w, `{"document":["required"]}`, http.StatusBadRequest)
		return
	}
	body, _ := io.ReadAll(f)
	s.mu.Lock()
	if s.FailUploads > 0 {
		s.FailUploads--
		s.mu.Unlock()
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	s.uploads = append(s.uploads, Upload{Filename: fh.Filename, Title: r.FormValue("title"), Tags: r.Form["tags"], Body: body, TaskID: id})
	s.tasks[id] = &task{id: id, filename: fh.Filename, title: r.FormValue("title"), created: time.Now(), outcome: s.Next}
	drop := s.DropAfter > 0
	if drop {
		s.DropAfter--
	}
	s.mu.Unlock()
	if drop {
		// Consumed, but the client never sees the answer.
		if hj, ok := w.(http.Hijacker); ok {
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(id)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, t := range s.tasks {
		if v := q.Get("task_id"); v != "" && v != t.id {
			continue
		}
		if v := q.Get("name"); v != "" && !strings.Contains(strings.ToLower(t.filename), strings.ToLower(v)) {
			continue
		}
		if q.Get("task_id") != "" {
			t.polls++
		}
		out = append(out, s.render(t))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"count": len(out), "next": nil, "previous": nil, "results": out})
}

func (s *Server) render(t *task) map[string]any {
	m := map[string]any{
		"task_id":      t.id,
		"task_type":    "consume_file",
		"date_created": t.created.Format(time.RFC3339),
		"input_data":   map[string]any{"filename": t.filename, "mime_type": "application/pdf"},
		"status":       "started",
		"result_data":  nil,
	}
	if t.polls <= s.PendingFor {
		return m
	}
	switch t.outcome {
	case Succeed:
		if t.docID == 0 {
			s.nextDoc++
			t.docID = s.nextDoc
			// Like Paperless: without a title, the file name becomes one.
			title := t.title
			if title == "" {
				title = strings.TrimSuffix(t.filename, ".pdf")
			}
			s.docs[t.docID] = title
		}
		m["status"], m["result_data"] = "success", map[string]any{"document_id": t.docID}
	case Duplicate:
		m["status"], m["result_data"] = "failure", map[string]any{"duplicate_of": 42, "duplicate_in_trash": false}
	case Fail:
		m["status"], m["result_data"] = "failure", map[string]any{"error_type": "ConsumerError", "error_message": "PDF is broken"}
	case Stopped:
		m["status"], m["result_data"] = "success", map[string]any{"reason": "Barcode splitting complete"}
	}
	return m
}

func (s *Server) listDocuments(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docRequests++
	if s.FailDocumentsAfter > 0 && s.docRequests > s.FailDocumentsAfter {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	if s.ForbidDocuments {
		http.Error(w, `{"detail":"You do not have permission to perform this action."}`, http.StatusForbidden)
		return
	}
	var out []map[string]any
	for _, v := range strings.Split(r.URL.Query().Get("id__in"), ",") {
		id, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		if title, ok := s.docs[id]; ok {
			out = append(out, map[string]any{"id": id, "title": title})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"count": len(out), "next": nil, "previous": nil, "results": out})
}

// DocumentRequests counts calls to /api/documents/.
func (s *Server) DocumentRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docRequests
}

// SetTitle renames a document, as triage in Paperless would.
func (s *Server) SetTitle(docID int, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[docID] = title
}

// DeleteDocument removes a document (or hides it from the token).
func (s *Server) DeleteDocument(docID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, docID)
}

// Uploads returns what was received.
func (s *Server) Uploads() []Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Upload(nil), s.uploads...)
}

// Set changes behaviour under the lock.
func (s *Server) Set(f func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}
