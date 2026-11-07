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

	"github.com/guigui42/scan-hub/internal/config"
	"github.com/guigui42/scan-hub/internal/escl"
	"github.com/guigui42/scan-hub/internal/paperless"
	"github.com/guigui42/scan-hub/internal/pdf"
	"github.com/guigui42/scan-hub/internal/pipeline"
	"github.com/guigui42/scan-hub/internal/scansnap"
	"github.com/guigui42/scan-hub/internal/session"
	"github.com/guigui42/scan-hub/internal/vens"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Config of the web UI.
type Config struct {
	AllowedHosts []string
	KeyPath      string // CSRF HMAC key file
	ScanSnap     vens.ScanConfig
	Epson        config.EpsonProfile
	Version      string // cache-busting for /static
	PaperlessURL string // for links to filed documents
	Logger       *slog.Logger
}

// Deps are the services the UI drives; nil ones are shown as absent.
type Deps struct {
	ScanSnap  *scansnap.Manager
	Epson     *escl.Client
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

	epsonCache cached[epsonStatus]
	plCache    cached[plStatus]
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

type epsonStatus struct{ State, Detail string }

type plStatus struct {
	Reachable bool
	Detail    string
}

type statusView struct {
	HasScanSnap  bool
	ScanSnap     scansnap.Status
	HasEpson     bool
	Epson        epsonStatus
	HasPaperless bool
	Paperless    plStatus
	Waiting      int // outbox items not yet delivered
	Failed       int
}

func (s *Server) status(reqCtx context.Context) statusView {
	// Cached probes must not inherit the request's cancellation, or an
	// aborted poll would be cached as "unreachable".
	ctx := context.WithoutCancel(reqCtx)
	v := statusView{HasScanSnap: s.d.ScanSnap != nil, HasEpson: s.d.Epson != nil, HasPaperless: s.d.Outbox != nil}
	if v.HasScanSnap {
		v.ScanSnap = s.d.ScanSnap.Status()
	}
	if v.HasEpson {
		v.Epson = s.epsonCache.get(10*time.Second, func() epsonStatus {
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			st, err := s.d.Epson.Status(c)
			if err != nil {
				return epsonStatus{State: "Unreachable", Detail: err.Error()}
			}
			return epsonStatus{State: st.State}
		})
	}
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
	if sc := r.PostFormValue("scanner"); sc == "scansnap" || sc == "epson" {
		to += "?scanner=" + sc
	}
	redirect(w, r, to)
}

type sessionView struct {
	M              session.Manifest
	DefaultScanner string
	Job            *Job
	SS             struct {
		Color, DPI, Paper string
		Duplex, Blank     bool
	}
	EP          struct{ Color, DPI, Region string }
	HasScanSnap bool
	HasEpson    bool
	HasOutbox   bool
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
	v := sessionView{M: m, HasScanSnap: s.d.ScanSnap != nil, HasEpson: s.d.Epson != nil, HasOutbox: s.d.Outbox != nil}
	switch sc := r.URL.Query().Get("scanner"); {
	case sc == "epson" && v.HasEpson, !v.HasScanSnap && v.HasEpson:
		v.DefaultScanner = "epson"
	default:
		v.DefaultScanner = "scansnap"
	}
	v.SS.Color, v.SS.DPI, v.SS.Paper = config.ScanSnapProfileStrings(s.cfg.ScanSnap)
	v.SS.Duplex, v.SS.Blank = s.cfg.ScanSnap.Duplex, s.cfg.ScanSnap.BlankPageRemoval
	v.EP.Color, v.EP.DPI, v.EP.Region = config.EpsonProfileStrings(s.cfg.Epson)
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
	var (
		job Job
		err error
	)
	switch r.PostFormValue("scanner") {
	case "scansnap":
		if s.d.ScanSnap == nil {
			s.fail(w, r, http.StatusBadRequest, "The ScanSnap is not configured.")
			return
		}
		cfg, perr := config.ParseScanSnapProfile(r.PostFormValue("ss_color"), r.PostFormValue("ss_dpi"),
			r.PostFormValue("ss_paper"), r.PostFormValue("ss_duplex") == "on", r.PostFormValue("ss_blank") == "on")
		if perr != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "Invalid ScanSnap settings.")
			return
		}
		if st := s.d.ScanSnap.Status(); st.State != scansnap.StateClaimed {
			s.fail(w, r, http.StatusConflict, scanSnapNotReady(st))
			return
		}
		job, err = s.jobs.start("scansnap", id, func(ctx context.Context, added func()) error {
			res := s.d.ScanSnap.Scan(ctx, scansnap.ScanRequest{Config: cfg, OnPage: func(p scansnap.Page) error {
				if _, err := s.d.Store.AddPage(id, p.Data, "scansnap", p.Sheet, p.Side); err != nil {
					return err
				}
				added()
				return nil
			}})
			return friendlyScanError(res.Err)
		})
	case "epson":
		if s.d.Epson == nil {
			s.fail(w, r, http.StatusBadRequest, "The Epson is not configured.")
			return
		}
		prof, perr := config.ParseEpsonProfile(r.PostFormValue("ep_color"), r.PostFormValue("ep_dpi"), r.PostFormValue("ep_region"))
		if perr != nil {
			s.fail(w, r, http.StatusUnprocessableEntity, "Invalid Epson settings.")
			return
		}
		job, err = s.jobs.start("epson", id, func(ctx context.Context, added func()) error {
			docs, err := s.d.Epson.Scan(ctx, EpsonSettings(prof))
			if err != nil {
				return friendlyScanError(err)
			}
			for _, d := range docs {
				if _, err := s.d.Store.AddPage(id, d.Data, "epson", 0, 0); err != nil {
					return err
				}
				added()
			}
			return nil
		})
	default:
		s.fail(w, r, http.StatusBadRequest, "Pick a scanner.")
		return
	}
	if errors.Is(err, errSessionBusy) {
		s.fail(w, r, http.StatusConflict, "This session is being sent or discarded.")
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusConflict, "A scan is already running on that scanner.")
		return
	}
	s.log.Info("scan started", "session", id, "scanner", job.Scanner, "job", job.ID)
	s.fragment(w, http.StatusAccepted, "job", job)
}

// EpsonSettings maps a profile to eSCL settings.
func EpsonSettings(p config.EpsonProfile) escl.ScanSettings {
	st := escl.ScanSettings{ColorMode: p.ColorMode, DPI: p.DPI}
	switch p.Region {
	case "a4":
		st.Region = escl.RegionA4
	case "letter":
		st.Region = escl.RegionLetter
	}
	return st
}

func scanSnapNotReady(st scansnap.Status) string {
	switch st.State {
	case scansnap.StateScanning:
		return "The ScanSnap is already scanning."
	case scansnap.StateHeldByOther:
		if st.Holder != "" {
			return "The ScanSnap is in use by " + st.Holder + "."
		}
		return "The ScanSnap is in use by another computer."
	case scansnap.StateBadPassword:
		return "The ScanSnap rejected the configured password."
	default:
		return "The ScanSnap is not connected yet (" + string(st.State) + ")."
	}
}

func friendlyScanError(err error) error {
	var se *vens.ScanError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		switch se.Kind {
		case vens.ScanErrNoPaper:
			return errors.New("No paper in the feeder.")
		case vens.ScanErrPaperJam:
			return errors.New("Paper jam: clear the feeder.")
		case vens.ScanErrCoverOpen:
			return errors.New("The feeder cover is open.")
		case vens.ScanErrMultiFeed:
			return errors.New("Several sheets fed at once: pages up to that point were kept.")
		}
	case errors.Is(err, scansnap.ErrBusy), errors.Is(err, escl.ErrBusy):
		return errors.New("The scanner is busy.")
	case errors.Is(err, scansnap.ErrNotReady):
		return errors.New("The ScanSnap is not connected.")
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
	out := map[string]any{}
	if v.HasScanSnap {
		out["scansnap"] = v.ScanSnap
	}
	if v.HasEpson {
		out["epson"] = map[string]string{"state": v.Epson.State, "detail": v.Epson.Detail}
	}
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
		"ssLabel": func(st scansnap.State) string {
			return map[scansnap.State]string{
				scansnap.StateClaimed: "Ready", scansnap.StateScanning: "Scanning", scansnap.StateConnecting: "Connecting",
				scansnap.StateHeldByOther: "In use", scansnap.StateBadPassword: "Wrong password",
				scansnap.StateNotConnected: "Offline", scansnap.StateStopping: "Stopping", scansnap.StateStopped: "Stopped",
			}[st]
		},
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
		"ssTone": func(st scansnap.State) string {
			switch st {
			case scansnap.StateClaimed:
				return "ok"
			case scansnap.StateScanning, scansnap.StateConnecting:
				return "busy"
			case scansnap.StateHeldByOther, scansnap.StateBadPassword:
				return "bad"
			}
			return "idle"
		},
		"epTone": func(st string) string {
			switch st {
			case "Idle":
				return "ok"
			case "Processing", "Testing":
				return "busy"
			case "Unreachable", "Down", "Stopped":
				return "bad"
			}
			return "idle"
		},
		"epLabel": func(st string) string {
			if st == "Idle" {
				return "Ready"
			}
			return st
		},
		"thumbH": func(w, h int) int {
			if w <= 0 || h <= 0 {
				return 509
			}
			return h * 360 / w
		},
		"scannerLabel": func(sc string) string {
			return map[string]string{"scansnap": "ScanSnap", "epson": "Epson"}[sc]
		},
		"docURL": func(id int) string {
			if s.cfg.PaperlessURL == "" {
				return ""
			}
			return fmt.Sprintf("%s/documents/%d/details", strings.TrimRight(s.cfg.PaperlessURL, "/"), id)
		},
		"sourceLabel": func(src string) string {
			return map[string]string{"ui": "Desk", "scansnap-button": "ScanSnap button", "epson": "Epson", "recovered": "Recovered"}[src]
		},
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
