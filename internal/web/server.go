// Package web is the mobile-first UI: server-rendered html/template pages
// with htmx 4 for partial updates and Alpine.js (CSP build) for local state.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/guigui42/scan-hub/internal/paperless"
	"github.com/guigui42/scan-hub/internal/pdf"
	"github.com/guigui42/scan-hub/internal/pipeline"
	"github.com/guigui42/scan-hub/internal/scanner"
	"github.com/guigui42/scan-hub/internal/session"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Config of the web UI.
type Config struct {
	AllowedHosts []string
	KeyPath      string // CSRF HMAC key file
	Version      string // cache-busting for /static
	PaperlessURL string // for links to filed documents
	Logger       *slog.Logger
}

// Deps are the services the UI drives; nil ones are shown as absent.
type Deps struct {
	Scanners  []scanner.Device // in display order
	Store     *session.Store
	Outbox    *paperless.Outbox
	Paperless *paperless.Client
	Ready     func() error // extra readiness checks (workers running)
	DataDir   string       // must be writable for /readyz
}

// Server serves the UI.
type Server struct {
	cfg   Config
	d     Deps
	log   *slog.Logger
	key   []byte
	pages map[string]*template.Template
	jobs  *jobs

	plCache cached[plStatus]
}

// New builds the server. ctx bounds background scan jobs.
func New(ctx context.Context, cfg Config, d Deps) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	key, err := loadKey(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("csrf key: %w", err)
	}
	if cfg.Version == "" {
		cfg.Version = assetVersion()
	}
	s := &Server{cfg: cfg, d: d, log: cfg.Logger.With("component", "web"), key: key, jobs: newJobs(ctx)}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// Wait blocks until background scan jobs have finished (after ctx ends).
func (s *Server) Wait() { s.jobs.wait() }

// Handler returns the HTTP handler with the security middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /status", s.statusJSON)

	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /fragments/status", s.statusFragment)
	mux.HandleFunc("GET /fragments/desk", s.deskFragment)
	mux.HandleFunc("POST /scanners/{id}/release", s.releaseScanner(true))
	mux.HandleFunc("POST /scanners/{id}/take-back", s.releaseScanner(false))
	mux.HandleFunc("POST /sessions", s.createSession)
	mux.HandleFunc("GET /sessions/{id}", s.sessionPage)
	mux.HandleFunc("GET /sessions/{id}/pages", s.pagesFragment)
	mux.HandleFunc("POST /sessions/{id}/title", s.setTitle)
	mux.HandleFunc("POST /sessions/{id}/scan", s.startScan)
	mux.HandleFunc("POST /sessions/{id}/send", s.sendSession)
	mux.HandleFunc("GET /sessions/{id}/pdf", s.sessionPDF)
	mux.HandleFunc("DELETE /sessions/{id}", s.deleteSession)
	mux.HandleFunc("DELETE /sessions/{id}/pages/{file}", s.deletePage)
	mux.HandleFunc("GET /sessions/{id}/pages/{file}/thumb", s.pageImage(true))
	mux.HandleFunc("GET /sessions/{id}/pages/{file}/image", s.pageImage(false))
	mux.HandleFunc("GET /jobs/{id}", s.jobFragment)
	mux.HandleFunc("GET /outbox", s.outboxPage)
	mux.HandleFunc("GET /fragments/outbox", s.outboxFragment)
	mux.HandleFunc("POST /outbox/{id}/retry", s.outboxAction(true))
	mux.HandleFunc("POST /outbox/{id}/discard", s.outboxAction(false))
	mux.HandleFunc("GET /outbox/{id}/pdf", s.outboxPDF)
	return s.secure(mux)
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		h.ServeHTTP(w, r)
	})
}

// --- status models (cached so polling doesn't hammer the devices) ---

type cached[T any] struct {
	mu  sync.Mutex
	at  time.Time
	val T
}

func (c *cached[T]) get(ttl time.Duration, load func() T) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > ttl {
		c.val, c.at = load(), time.Now()
	}
	return c.val
}

type plStatus struct {
	Reachable bool
	Detail    string
}

// deviceView is one scanner card or ticket.
type deviceView struct {
	ID, Name   string
	Kind       scanner.Kind
	Status     scanner.Status
	HasButton  bool
	CanRelease bool
	Released   bool
}

type statusView struct {
	Scanners     []deviceView
	HasPaperless bool
	Paperless    plStatus
	Waiting      int // outbox items not yet delivered
	Failed       int
}

func (s *Server) status(reqCtx context.Context) statusView {
	// Cached probes must not inherit the request's cancellation, or an
	// aborted poll would be cached as "unreachable".
	ctx := context.WithoutCancel(reqCtx)
	v := statusView{Scanners: make([]deviceView, len(s.d.Scanners)), HasPaperless: s.d.Outbox != nil}
	// Probe concurrently: one slow scanner must not delay the others.
	var wg sync.WaitGroup
	for i, d := range s.d.Scanners {
		_, hasButton := d.(scanner.Button)
		rel, canRelease := d.(scanner.Releaser)
		v.Scanners[i] = deviceView{ID: d.ID(), Name: d.Name(), Kind: d.Kind(), HasButton: hasButton, CanRelease: canRelease}
		if canRelease {
			v.Scanners[i].Released = rel.Released()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.Scanners[i].Status = d.Status(ctx)
		}()
	}
	defer wg.Wait()
	if v.HasPaperless {
		v.Paperless = s.plCache.get(30*time.Second, func() plStatus {
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if err := s.d.Paperless.Ping(c); err != nil {
				return plStatus{Detail: err.Error()}
			}
			return plStatus{Reachable: true}
		})
		for _, it := range s.d.Outbox.List() {
			switch {
			case it.State == paperless.StateFailure:
				v.Failed++
			case !it.State.Done():
				v.Waiting++
			}
		}
	}
	return v
}

// --- pages ---

type pageData struct {
	Title   string
	Nav     string
	CSRF    string
	V       string
	Status  statusView
	Content any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, title, nav string, content any) {
	s.renderStatus(w, r, http.StatusOK, page, title, nav, content)
}

func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, page, title, nav string, content any) {
	d := pageData{Title: title, Nav: nav, CSRF: s.ensureCSRF(w, r), V: s.cfg.Version, Status: s.status(r.Context()), Content: content}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := s.pages[page].ExecuteTemplate(w, "layout", d); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

func (s *Server) fragment(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := s.pages["fragments"].ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render fragment", "name", name, "err", err)
	}
}

// fail answers with an error fragment, which htmx swaps into #flash
// (hx-status on <body>). Full-page requests get a proper error page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if status >= 500 {
		s.log.Error("request failed", "path", r.URL.Path, "status", status, "msg", msg)
	}
	if r.Header.Get("HX-Request") == "true" || r.Method != http.MethodGet {
		s.fragment(w, status, "flash", map[string]string{"Kind": "error", "Msg": msg})
		return
	}
	headline := "Not found"
	if status >= 500 {
		headline = "That didn't work"
	} else if status != http.StatusNotFound {
		headline = "Not possible"
	}
	s.renderStatus(w, r, status, "error", headline, "", map[string]string{"Headline": headline, "Msg": msg})
}

// redirect navigates the browser (htmx: HX-Redirect; plain: 303).
func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

type homeView struct {
	Sessions []session.Manifest
	Recent   []paperless.Item
}

func (s *Server) homeModel() (homeView, error) {
	list, err := s.d.Store.List()
	if err != nil {
		return homeView{}, err
	}
	v := homeView{Sessions: list}
	if s.d.Outbox != nil {
		items := s.d.Outbox.List()
		v.Recent = items[:min(len(items), 5)]
	}
	return v, nil
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	v, err := s.homeModel()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Could not list sessions.")
		return
	}
	s.render(w, r, "home", "Desk", "desk", v)
}

// deskFragment refreshes the session and outbox lists, so batches from the
// scanner's own button appear without a reload.
func (s *Server) deskFragment(w http.ResponseWriter, r *http.Request) {
	v, err := s.homeModel()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Could not list sessions.")
		return
	}
	s.fragment(w, http.StatusOK, "desk", v)
}

func (s *Server) statusFragment(w http.ResponseWriter, r *http.Request) {
	s.fragment(w, http.StatusOK, "status", s.status(r.Context()))
}

// releaseScanner lets another computer use a scanner, or takes it back, and
// answers with the refreshed device cards.
func (s *Server) releaseScanner(release bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dev, ok := scanner.Find(s.d.Scanners, r.PathValue("id"))
		rel, can := dev.(scanner.Releaser)
		if !ok || !can {
			s.fail(w, r, http.StatusNotFound, "That scanner can't be released.")
			return
		}
		var err error
		if release {
			err = rel.Release(r.Context())
		} else {
			err = rel.TakeBack(r.Context())
		}
		switch {
		case errors.Is(err, scanner.ErrBusy):
			s.fail(w, r, http.StatusConflict, err.Error())
			return
		case err != nil:
			s.fail(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		s.log.Info("scanner "+map[bool]string{true: "released", false: "taken back"}[release], "scanner", dev.ID())
		s.fragment(w, http.StatusOK, "status", s.status(r.Context()))
	}
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "Bad form.")
		return
	}
	m, err := s.d.Store.Create("ui")
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Could not create a session.")
		return
	}
	to := "/sessions/" + m.ID
	if sc := r.PostFormValue("scanner"); sc != "" {
		if _, ok := scanner.Find(s.d.Scanners, sc); ok {
			to += "?scanner=" + sc
		}
	}
	redirect(w, r, to)
}

type sessionView struct {
	M         session.Manifest
	Active    string // scanner shown first
	Job       *Job
	Scanners  []formView
	HasOutbox bool
}

// formView is the scan form of one scanner.
type formView struct {
	ID, Name string
	Kind     scanner.Kind
	Fields   []scanner.Field
}

func (s *Server) loadSession(w http.ResponseWriter, r *http.Request) (session.Manifest, bool) {
	m, err := s.d.Store.Get(r.PathValue("id"))
	if errors.Is(err, session.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, "This session no longer exists (sent or discarded).")
		return m, false
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Could not read the session.")
		return m, false
	}
	return m, true
}

func (s *Server) sessionPage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	v := sessionView{M: m, HasOutbox: s.d.Outbox != nil}
	for _, d := range s.d.Scanners {
		v.Scanners = append(v.Scanners, formView{ID: d.ID(), Name: d.Name(), Kind: d.Kind(), Fields: d.Form()})
	}
	if sc := r.URL.Query().Get("scanner"); sc != "" {
		if _, ok := scanner.Find(s.d.Scanners, sc); ok {
			v.Active = sc
		}
	}
	if v.Active == "" && len(v.Scanners) > 0 {
		v.Active = v.Scanners[0].ID
	}
	if j, ok := s.jobs.runningFor(m.ID); ok {
		v.Job = &j
	}
	s.render(w, r, "session", "Session", "desk", v)
}

func (s *Server) pagesFragment(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	s.fragment(w, http.StatusOK, "pages", sessionView{M: m, HasOutbox: s.d.Outbox != nil})
}

func (s *Server) setTitle(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.loadSession(w, r); !ok {
		return
	}
	title := strings.TrimSpace(r.PostFormValue("title"))
	if _, err := s.d.Store.SetTitle(r.PathValue("id"), title); err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "Title too long (200 characters max).")
		return
	}
	s.fragment(w, http.StatusOK, "saved", nil)
}

func (s *Server) startScan(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	id := m.ID
	dev, found := scanner.Find(s.d.Scanners, r.PostFormValue("scanner"))
	if !found {
		s.fail(w, r, http.StatusBadRequest, "Pick a scanner.")
		return
	}
	settings, err := dev.ParseForm(r.PostFormValue)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "Invalid settings for "+dev.Name()+".")
		return
	}
	if err := dev.Ready(); err != nil {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	job, err := s.jobs.start(dev.ID(), string(dev.Kind()), id, func(ctx context.Context, added func()) error {
		err := dev.Scan(ctx, settings, func(p scanner.Page) error {
			if _, err := s.d.Store.AddPage(id, p.Data, dev.ID(), p.Sheet, p.Side); err != nil {
				return err
			}
			added()
			return nil
		})
		return friendlyScanError(dev, err)
	})
	if errors.Is(err, errSessionBusy) {
		s.fail(w, r, http.StatusConflict, "This session is busy: wait for its scan, send or discard to finish.")
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusConflict, "A scan is already running on that scanner.")
		return
	}
	s.log.Info("scan started", "session", id, "scanner", job.Scanner, "job", job.ID)
	s.fragment(w, http.StatusAccepted, "job", job)
}

// friendlyScanError turns a scan error into a message for the job panel.
func friendlyScanError(dev scanner.Device, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, scanner.ErrNoPaper):
		return errors.New("No paper in the feeder.")
	case errors.Is(err, scanner.ErrJam):
		return errors.New("Paper jam: clear the feeder.")
	case errors.Is(err, scanner.ErrCoverOpen):
		return errors.New("The feeder cover is open.")
	case errors.Is(err, scanner.ErrMultiFeed):
		return errors.New("Several sheets fed at once: pages up to that point were kept.")
	case errors.Is(err, scanner.ErrBusy):
		return errors.New(dev.Name() + " is busy.")
	case errors.Is(err, scanner.ErrNotReady):
		return errors.New(dev.Name() + " is not connected.")
	case errors.Is(err, session.ErrLimit):
		return errors.New("Session full: " + err.Error())
	case errors.Is(err, context.Canceled):
		return errors.New("Scan cancelled (server shutting down).")
	}
	return err
}

func (s *Server) jobFragment(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.get(r.PathValue("id"))
	if !ok {
		s.fragment(w, http.StatusOK, "job-gone", nil)
		return
	}
	if seen := r.URL.Query().Get("seen"); seen != fmt.Sprint(job.Pages) || job.State != JobRunning {
		w.Header().Set("HX-Trigger", "pages-changed")
	}
	s.fragment(w, http.StatusOK, "job", job)
}

func (s *Server) deletePage(w http.ResponseWriter, r *http.Request) {
	m, err := s.d.Store.DeletePage(r.PathValue("id"), r.PathValue("file"))
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "That page is already gone.")
		return
	}
	s.fragment(w, http.StatusOK, "pages", sessionView{M: m, HasOutbox: s.d.Outbox != nil})
}

func (s *Server) pageImage(thumb bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := s.d.Store.Get(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		for _, p := range m.Pages {
			if p.File != r.PathValue("file") {
				continue
			}
			path, ctype := s.d.Store.PagePath(m.ID, p), p.MIME
			// Browsers can't show TIFF: the full view of a B&W page is its thumbnail.
			if (thumb || p.MIME == "image/tiff") && p.Thumb != "" {
				path, ctype = s.d.Store.ThumbPath(m.ID, p), "image/jpeg"
			}
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
			http.ServeFile(w, r, path)
			return
		}
		http.NotFound(w, r)
	}
}

// sessionPDF downloads the merged PDF of a session (read-only: the session
// stays on the desk).
func (s *Server) sessionPDF(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	if len(m.Pages) == 0 {
		s.fail(w, r, http.StatusUnprocessableEntity, "Scan at least one page first.")
		return
	}
	var buf bytes.Buffer
	if err := pdf.Write(&buf, pipeline.PDFPages(s.d.Store, m), pdf.Options{Title: m.Title, Created: m.Created}); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Could not build the PDF: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": pdfFilename(m)}))
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// pdfFilename derives a safe download name from the title or the date.
func pdfFilename(m session.Manifest) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), strings.ContainsRune(" -_.,()", r):
			return r
		}
		return '_'
	}, strings.TrimSpace(m.Title))
	name = strings.Trim(name, " ._")
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	if name == "" {
		name = "scan-" + m.Created.Local().Format("2006-01-02-1504")
	}
	return name + ".pdf"
}

func (s *Server) sendSession(w http.ResponseWriter, r *http.Request) {
	if s.d.Outbox == nil {
		s.fail(w, r, http.StatusBadRequest, "Paperless is not configured.")
		return
	}
	id := r.PathValue("id")
	unlock, err := s.jobs.lockSession(id)
	if err != nil {
		s.fail(w, r, http.StatusConflict, "Wait for the scan (or the other send) to finish first.")
		return
	}
	defer unlock()
	_, err = pipeline.Submit(s.log, s.d.Store, s.d.Outbox, id)
	switch {
	case errors.Is(err, pipeline.ErrEmpty):
		s.fail(w, r, http.StatusUnprocessableEntity, "Scan at least one page first.")
	case errors.Is(err, session.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "This session was already sent or discarded.")
	case errors.Is(err, paperless.ErrFull):
		s.fail(w, r, http.StatusInsufficientStorage, "The outbox is full. Free space or wait for uploads.")
	case err != nil:
		s.fail(w, r, http.StatusInternalServerError, "Could not build the PDF: "+err.Error())
	default:
		redirect(w, r, "/outbox")
	}
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	unlock, err := s.jobs.lockSession(id)
	if err != nil {
		s.fail(w, r, http.StatusConflict, "Wait for the scan (or the send) to finish first.")
		return
	}
	defer unlock()
	if err := s.d.Store.Delete(id); err != nil && !errors.Is(err, session.ErrNotFound) {
		s.fail(w, r, http.StatusInternalServerError, "Could not discard the session.")
		return
	}
	redirect(w, r, "/")
}

type outboxView struct {
	Items  []paperless.Item
	Active bool // something still in flight: keep polling
}

func (s *Server) outboxModel() outboxView {
	v := outboxView{}
	if s.d.Outbox == nil {
		return v
	}
	v.Items = s.d.Outbox.List()
	for _, it := range v.Items {
		if !it.State.Done() && it.State != paperless.StateFailure {
			v.Active = true
		}
	}
	return v
}

func (s *Server) outboxPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "outbox", "Outbox", "outbox", s.outboxModel())
}

func (s *Server) outboxFragment(w http.ResponseWriter, r *http.Request) {
	s.fragment(w, http.StatusOK, "outbox-list", s.outboxModel())
}

func (s *Server) outboxAction(retry bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.d.Outbox == nil {
			s.fail(w, r, http.StatusBadRequest, "Paperless is not configured.")
			return
		}
		var err error
		if retry {
			_, err = s.d.Outbox.Retry(r.PathValue("id"))
		} else {
			err = s.d.Outbox.Discard(r.PathValue("id"))
		}
		switch {
		case errors.Is(err, paperless.ErrNotFound):
			s.fail(w, r, http.StatusNotFound, "That item no longer exists.")
		case errors.Is(err, paperless.ErrState):
			s.fail(w, r, http.StatusConflict, "Only failed items can be retried or discarded.")
		case err != nil:
			s.fail(w, r, http.StatusInternalServerError, err.Error())
		default:
			s.fragment(w, http.StatusOK, "outbox-list", s.outboxModel())
		}
	}
}

func (s *Server) outboxPDF(w http.ResponseWriter, r *http.Request) {
	if s.d.Outbox == nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.d.Outbox.PDFPath(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="scan.pdf"`)
	http.ServeFile(w, r, p)
}

// statusJSON reports devices and Paperless (degraded states never fail
// readiness; scanning works during a Paperless outage).
func (s *Server) statusJSON(w http.ResponseWriter, r *http.Request) {
	v := s.status(r.Context())
	scanners := map[string]any{}
	for _, d := range v.Scanners {
		scanners[d.ID] = map[string]any{
			"name": d.Name, "kind": d.Kind, "state": d.Status.Label, "tone": d.Status.Tone,
			"detail": d.Status.Detail, "device": d.Status.Raw,
		}
	}
	out := map[string]any{"scanners": scanners}
	if v.HasPaperless {
		out["paperless"] = map[string]any{"reachable": v.Paperless.Reachable, "detail": v.Paperless.Detail, "waiting": v.Waiting, "failed": v.Failed}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// assetVersion fingerprints the embedded CSS/JS for cache busting.
func assetVersion() string {
	h := sha256.New()
	for _, f := range []string{"static/app.css", "static/app.js"} {
		b, _ := staticFS.ReadFile(f)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}

// readyz: /data writable and background workers alive. Scanner and
// Paperless reachability are reported by /status, never here.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	f, err := os.CreateTemp(s.d.DataDir, ".readyz-*")
	if err == nil {
		name := f.Name()
		f.Close()
		os.Remove(name)
	}
	if err == nil && s.d.Ready != nil {
		err = s.d.Ready()
	}
	if err != nil {
		http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ready\n"))
}

// --- templates ---

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"short": func(id string) string { return id[max(0, len(id)-6):] },
		"bytes": humanBytes,
		"ago":   ago,
		"inc":   func(i int) int { return i + 1 },
		"stamp": stampClass,
		"stateLabel": func(st paperless.State) string {
			return map[paperless.State]string{
				paperless.StatePending: "Queued", paperless.StatePosting: "Uploading", paperless.StateUncertain: "Checking",
				paperless.StateAccepted: "Processing", paperless.StateSuccess: "Filed", paperless.StateDuplicate: "Already filed",
				paperless.StateFailure: "Failed",
			}[st]
		},
		"isFailure": func(st paperless.State) bool { return st == paperless.StateFailure },
		"hasPDF":    func(st paperless.State) bool { return !st.Done() },
		"eq2":       func(a, b string) bool { return a == b },
		"sel": func(a, b string) template.HTMLAttr {
			if a == b {
				return "selected"
			}
			return ""
		},
		"checked": func(b bool) template.HTMLAttr {
			if b {
				return "checked"
			}
			return ""
		},
		"thumbH": func(w, h int) int {
			if w <= 0 || h <= 0 {
				return 509
			}
			return h * 360 / w
		},
		"scannerLabel": s.scannerLabel,
		"kindLabel": func(k scanner.Kind) string {
			if k == scanner.Flatbed {
				return "Glass"
			}
			return "Feeder"
		},
		"isFlatbed": func(k scanner.Kind) bool { return k == scanner.Flatbed },
		"nameHead":  func(n string) string { h, _ := splitName(n); return h },
		"nameTail":  func(n string) string { _, t := splitName(n); return t },
		"bars":      func(n int) string { return strconv.Itoa(max(0, min(n, 3))) },
		"docURL": func(id int) string {
			if s.cfg.PaperlessURL == "" {
				return ""
			}
			return fmt.Sprintf("%s/documents/%d/details", strings.TrimRight(s.cfg.PaperlessURL, "/"), id)
		},
		"sourceLabel": s.sourceLabel,
	}
	base := template.New("").Funcs(funcs)
	base, err := base.ParseFS(templateFS, "templates/layout.html", "templates/fragments.html")
	if err != nil {
		return err
	}
	s.pages = map[string]*template.Template{"fragments": base}
	for _, p := range []string{"home", "session", "outbox", "error"} {
		t, err := base.Clone()
		if err != nil {
			return err
		}
		if _, err := t.ParseFS(templateFS, "templates/"+p+".html"); err != nil {
			return err
		}
		s.pages[p] = t
	}
	return nil
}

// scannerLabel names the scanner a page came from: its display name, or the
// raw id once that scanner is gone from the list.
func (s *Server) scannerLabel(id string) string {
	if d, ok := scanner.Find(s.d.Scanners, id); ok {
		return d.Name()
	}
	if l, ok := map[string]string{"scansnap": "ScanSnap", "epson": "Epson"}[id]; ok {
		return l // pages stored before the scanner list
	}
	return id
}

// sourceLabel describes how a session or outbox item started. Outbox items
// keep it for weeks, so the labels of earlier releases stay.
func (s *Server) sourceLabel(src string) string {
	if id, ok := strings.CutPrefix(src, "button:"); ok {
		return s.scannerLabel(id) + " button"
	}
	if id, ok := strings.CutPrefix(src, "cli:"); ok {
		return s.scannerLabel(id) + " (command line)"
	}
	if l, ok := map[string]string{"ui": "Desk", "scansnap-button": "ScanSnap button", "epson": "Epson", "recovered": "Recovered"}[src]; ok {
		return l
	}
	return src
}

// splitName sets the last word of a multi-word name apart (shown in italics).
func splitName(n string) (head, tail string) {
	if i := strings.LastIndexByte(n, ' '); i > 0 {
		return n[:i], n[i+1:]
	}
	return n, ""
}

func stampClass(st paperless.State) string {
	switch st {
	case paperless.StateSuccess, paperless.StateDuplicate:
		return "ok"
	case paperless.StateFailure:
		return "bad"
	default:
		return "busy"
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Local().Format("2 Jan 15:04")
}
