package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/guigui42/scan-hub/internal/escl"
	"github.com/guigui42/scan-hub/internal/escl/escltest"
	"github.com/guigui42/scan-hub/internal/paperless"
	"github.com/guigui42/scan-hub/internal/paperless/paperlesstest"
	"github.com/guigui42/scan-hub/internal/scanner"
	"github.com/guigui42/scan-hub/internal/scanner/escldrv"
	"github.com/guigui42/scan-hub/internal/scanner/scansnapdrv"
	"github.com/guigui42/scan-hub/internal/scansnap"
	"github.com/guigui42/scan-hub/internal/session"
	"github.com/guigui42/scan-hub/internal/vens"
	"github.com/guigui42/scan-hub/internal/vens/venstest"
)

type rig struct {
	t      *testing.T
	srv    *httptest.Server
	c      *http.Client
	csrf   string
	ss     *venstest.Scanner
	pl     *paperlesstest.Server
	ob     *paperless.Outbox
	store  *session.Store
	ws     *Server
	origin string
}

func freeUDP(t *testing.T) int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func newRig(t *testing.T) *rig {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	ss, err := venstest.Start(venstest.Options{Password: "4242", EndOfBatchQuirk: true})
	if err != nil {
		t.Fatal(err)
	}
	mgr := scansnap.New(scansnap.Config{
		ScannerIP: ss.IP(), Password: "4242", JournalPath: filepath.Join(dir, "j.json"),
		ControlPort: ss.ControlPort(), DataPort: ss.DataPort(), DiscoveryPort: ss.DiscoveryPort(),
		DiscoveryReplyPort: freeUDP(t), NotifyPort: freeUDP(t),
		HeartbeatInterval: 50 * time.Millisecond, StatusInterval: time.Second, RetryMin: 50 * time.Millisecond, Logger: log,
	})
	mgrDone := make(chan struct{})
	go func() { mgr.Run(ctx); close(mgrDone) }()

	ep := escltest.Start()
	epc, err := escl.New(escl.Options{BaseURL: ep.URL, TLSPin: escl.SPKIPin(ep.Certificate()), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	pl := paperlesstest.Start("tok")
	pc, _ := paperless.NewClient(pl.URL, "tok", nil)
	ob, err := paperless.Open(pc, paperless.Options{Dir: filepath.Join(dir, "outbox"), MinFreeBytes: -1,
		PollMin: 10 * time.Millisecond, PollMax: 20 * time.Millisecond, TitleRefresh: 10 * time.Millisecond, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	obDone := make(chan struct{})
	go func() { ob.Run(ctx); close(obDone) }()
	store, _ := session.Open(filepath.Join(dir, "sessions"), session.Options{MinFreeBytes: -1})

	devs := []scanner.Device{
		scansnapdrv.NewDevice("feeder", "ScanSnap iX500", mgr, vens.DefaultScanConfig(), vens.DefaultScanConfig()),
		escldrv.NewDevice("glass", "Epson ET-2750", epc, escldrv.DefaultProfile, 0),
	}
	ws, err := New(ctx, Config{
		KeyPath: filepath.Join(dir, "csrf.key"), Version: "test",
		PaperlessURL: "https://paperless.example", Logger: log,
	}, Deps{Scanners: devs, Store: store, Outbox: ob, Paperless: pc, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	ws.cfg.AllowedHosts = []string{srv.Listener.Addr().String()}
	srv.Config.Handler = ws.Handler()

	jar, _ := cookiejar.New(nil)
	r := &rig{t: t, srv: srv, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		ss: ss, pl: pl, ob: ob, store: store, ws: ws, origin: srv.URL}
	t.Cleanup(func() {
		srv.Close()
		cancel()
		ws.Wait()
		<-mgrDone
		<-obDone
		ob.Close()
		ss.Close()
		ep.Close()
		pl.Close()
	})
	deadline := time.Now().Add(5 * time.Second)
	for mgr.Status().State != scansnap.StateClaimed {
		if time.Now().After(deadline) {
			t.Fatalf("scansnap not claimed: %+v", mgr.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r
}

var csrfRe = regexp.MustCompile(`X-CSRF-Token(?:&#34;|")\s*:\s*(?:&#34;|")([A-Za-z0-9_-]+)`)

func (r *rig) get(path string) (int, string, http.Header) {
	r.t.Helper()
	resp, err := r.c.Get(r.srv.URL + path)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := csrfRe.FindSubmatch(b); m != nil {
		r.csrf = string(m[1])
	}
	return resp.StatusCode, string(b), resp.Header
}

func (r *rig) send(method, path string, form url.Values, hdr map[string]string) (int, string, http.Header) {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Origin", r.origin)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set(csrfHeader, r.csrf)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := r.c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestSecurity(t *testing.T) {
	r := newRig(t)
	code, body, h := r.get("/")
	if code != 200 || !strings.Contains(body, "What are we") || r.csrf == "" {
		t.Fatalf("home %d csrf=%q", code, r.csrf)
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") || h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers %v", h)
	}
	if strings.Contains(h.Get("Content-Security-Policy"), "unsafe") {
		t.Fatal("CSP allows unsafe sources")
	}

	req, _ := http.NewRequest("GET", r.srv.URL+"/", nil)
	req.Host = "evil.example"
	resp, _ := r.c.Do(req)
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host: %d", resp.StatusCode)
	}

	if code, _, _ := r.send("POST", "/sessions", nil, map[string]string{csrfHeader: "wrong"}); code != 403 {
		t.Fatalf("bad CSRF: %d", code)
	}
	if code, _, _ := r.send("POST", "/sessions", nil, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}); code != 403 {
		t.Fatalf("cross-site: %d", code)
	}
	if code, _, _ := r.send("POST", "/sessions", nil, map[string]string{"Origin": "", "Sec-Fetch-Site": ""}); code != 403 {
		t.Fatalf("missing Origin: %d", code)
	}
	code, body, _ = r.send("POST", "/sessions", nil, map[string]string{csrfHeader: ""})
	if code != 403 || !strings.Contains(body, "expired") || !strings.Contains(body, `class="flash`) {
		t.Fatalf("no CSRF: %d %s", code, body)
	}
	if code, _, _ := r.send("PUT", "/sessions", nil, nil); code != 405 {
		t.Fatalf("PUT: %d", code)
	}
	if code, _, _ := r.get("/healthz"); code != 200 {
		t.Fatal("healthz")
	}
	if code, _, _ := r.get("/readyz"); code != 200 {
		t.Fatal("readyz")
	}
}

func waitJob(t *testing.T, r *rig, body string) string {
	t.Helper()
	m := regexp.MustCompile(`hx-get="/jobs/([0-9a-f]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no job polling in %s", body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b, _ := r.get("/jobs/" + m[1] + "?seen=0")
		if !strings.Contains(b, "hx-get=") {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return ""
}

func TestDeskScanSendFlow(t *testing.T) {
	r := newRig(t)
	r.get("/")
	code, _, h := r.send("POST", "/sessions", url.Values{"scanner": {"glass"}}, nil)
	if code != http.StatusNoContent || !strings.HasPrefix(h.Get("HX-Redirect"), "/sessions/") {
		t.Fatalf("create %d %v", code, h)
	}
	loc := h.Get("HX-Redirect")
	sid := strings.TrimSuffix(strings.TrimPrefix(loc, "/sessions/"), "?scanner=glass")
	code, body, _ := r.get(loc)
	if code != 200 || !strings.Contains(body, `<div class="session" x-data="sessionPage" data-scanner="glass">`) || !strings.Contains(body, "Nothing on the desk") {
		t.Fatalf("session page %d", code)
	}

	// Epson pass.
	code, body, _ = r.send("POST", "/sessions/"+sid+"/scan", url.Values{"scanner": {"glass"}, "color": {"color"}, "dpi": {"300"}, "region": {"a4"}}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("epson scan %d %s", code, body)
	}
	if b := waitJob(t, r, body); !strings.Contains(b, "1 page added") {
		t.Fatalf("epson job: %s", b)
	}

	// ScanSnap duplex batch of 2 sheets.
	r.ss.LoadPaper(2)
	code, body, _ = r.send("POST", "/sessions/"+sid+"/scan", url.Values{"scanner": {"feeder"}, "color": {"auto"}, "dpi": {"300"}, "paper": {"auto"}, "duplex": {"on"}}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("scansnap scan %d %s", code, body)
	}
	if b := waitJob(t, r, body); !strings.Contains(b, "4 pages added") {
		t.Fatalf("scansnap job: %s", b)
	}

	_, body, _ = r.get("/sessions/" + sid + "/pages")
	files := regexp.MustCompile(`/pages/(page-[^/"]+)/thumb`).FindAllStringSubmatch(body, -1)
	if len(files) != 5 || !strings.Contains(body, "5 pages → one PDF") {
		t.Fatalf("pages: %d", len(files))
	}
	if code, _, h := r.get("/sessions/" + sid + "/pages/" + files[0][1] + "/thumb"); code != 200 || h.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb %d", code)
	}
	code, body, _ = r.send("DELETE", "/sessions/"+sid+"/pages/"+files[0][1], nil, nil)
	if code != 200 || !strings.Contains(body, "4 pages → one PDF") {
		t.Fatalf("delete page %d", code)
	}
	if code, body, _ := r.send("POST", "/sessions/"+sid+"/title", url.Values{"title": {"Facture <EDF>"}}, nil); code != 200 || !strings.Contains(body, "Saved") {
		t.Fatalf("title %d %s", code, body)
	}

	code, _, h = r.send("POST", "/sessions/"+sid+"/send", nil, nil)
	if code != http.StatusNoContent || h.Get("HX-Redirect") != "/outbox" {
		t.Fatalf("send %d %v", code, h)
	}
	if _, err := r.store.Get(sid); err == nil {
		t.Fatal("session survived send")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		items := r.ob.List()
		if len(items) == 1 && items[0].State == paperless.StateSuccess {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox %+v", items)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ups := r.pl.Uploads()
	if len(ups) != 1 || ups[0].Title != "Facture <EDF>" {
		t.Fatalf("uploads %+v", ups)
	}
	_, body, _ = r.get("/outbox")
	if !strings.Contains(body, "Facture &lt;EDF&gt;") || !strings.Contains(body, "Filed") || !strings.Contains(body, "https://paperless.example/documents/101/details") {
		t.Fatalf("outbox page:\n%s", body)
	}
	if strings.Contains(body, "Facture <EDF>") {
		t.Fatal("title not escaped")
	}

	// Renamed during triage in Paperless: the home page follows, with a link.
	r.pl.SetTitle(101, "EDF <novembre>")
	deadline = time.Now().Add(5 * time.Second)
	for {
		if r.ob.List()[0].PaperlessTitle == "EDF <novembre>" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("title not refreshed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, body, _ = r.get("/")
	recent := body[strings.Index(body, "Recently sent"):]
	if !strings.Contains(recent, "EDF &lt;novembre&gt;") || !strings.Contains(recent, `href="https://paperless.example/documents/101/details"`) {
		t.Fatalf("home page:\n%s", recent)
	}
}

func TestErrorsAndOutboxActions(t *testing.T) {
	r := newRig(t)
	r.get("/")
	_, _, h := r.send("POST", "/sessions", nil, nil)
	sid := strings.TrimPrefix(h.Get("HX-Redirect"), "/sessions/")

	code, body, _ := r.send("POST", "/sessions/"+sid+"/send", nil, nil)
	if code != 422 || !strings.Contains(body, "at least one page") {
		t.Fatalf("empty send %d %s", code, body)
	}
	code, body, _ = r.send("POST", "/sessions/"+sid+"/scan", url.Values{"scanner": {"feeder"}, "color": {"auto"}, "dpi": {"300"}, "paper": {"auto"}}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("scan %d", code)
	}
	if b := waitJob(t, r, body); !strings.Contains(b, "No paper in the feeder") {
		t.Fatalf("no-paper job: %s", b)
	}
	if code, body, _ := r.get("/sessions/nope"); code != 404 || !strings.Contains(body, "no longer exists") || !strings.Contains(body, "Back to the desk") {
		t.Fatalf("missing session page %d", code)
	}

	// A failed delivery can be retried and discarded from the UI.
	r.pl.Set(func(s *paperlesstest.Server) { s.Next = paperlesstest.Fail })
	r.ss.LoadPaper(1)
	_, body, _ = r.send("POST", "/sessions/"+sid+"/scan", url.Values{"scanner": {"feeder"}, "color": {"auto"}, "dpi": {"300"}, "paper": {"auto"}}, nil)
	waitJob(t, r, body)
	r.send("POST", "/sessions/"+sid+"/send", nil, nil)
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for id == "" {
		for _, it := range r.ob.List() {
			if it.State == paperless.StateFailure {
				id = it.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no failure")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, body, _ = r.get("/outbox")
	if !strings.Contains(body, "/outbox/"+id+"/retry") || !strings.Contains(body, "PDF is broken") {
		t.Fatal("failure actions missing")
	}
	if code, _, h := r.get("/outbox/" + id + "/pdf"); code != 200 || h.Get("Content-Type") != "application/pdf" {
		t.Fatalf("pdf %d", code)
	}
	if code, body, _ := r.send("POST", "/outbox/"+id+"/discard", nil, nil); code != 200 || strings.Contains(body, id) {
		t.Fatalf("discard %d", code)
	}
	if code, _, _ := r.send("POST", "/outbox/"+id+"/retry", nil, nil); code != 404 {
		t.Fatalf("retry after discard %d", code)
	}
}

func TestVendoredAssetsMatchChecksums(t *testing.T) {
	sums, err := fs.ReadFile(staticFS, "static/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		want, name, _ := strings.Cut(line, "  ")
		b, err := fs.ReadFile(staticFS, "static/"+name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatalf("%s changed", name)
		}
		n++
	}
	if n != 7 {
		t.Fatalf("checked %d assets", n)
	}
}

func TestStaticServed(t *testing.T) {
	r := newRig(t)
	for path, ctype := range map[string]string{
		"/static/app.css":                                    "text/css",
		"/static/app.js":                                     "text/javascript",
		"/static/vendor/htmx-4.0.1.min.js":                   "text/javascript",
		"/static/fonts/ibm-plex-mono-latin-400-normal.woff2": "font/woff2",
	} {
		code, _, h := r.get(path)
		if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), ctype) {
			t.Fatalf("%s: %d %s", path, code, h.Get("Content-Type"))
		}
	}
}

func TestStatusCacheIgnoresAbortedRequests(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the browser went away before the probe ran
	v := r.ws.status(ctx)
	if len(v.Scanners) != 2 || v.Scanners[1].Status.Label != "Ready" || !v.Paperless.Reachable {
		t.Fatalf("aborted request poisoned the cache: %+v", v)
	}
}

func TestDeskFragmentShowsButtonSessions(t *testing.T) {
	r := newRig(t)
	_, body, _ := r.get("/")
	if !strings.Contains(body, `hx-get="/fragments/desk"`) {
		t.Fatal("desk lists are not polled")
	}
	m, _ := r.store.Create("button:feeder") // as the physical button does
	r.store.AddPage(m.ID, venstest.PageJPEG(0, 0), "feeder", 0, 0)
	old, _ := r.store.Create("scansnap-button") // stored by an earlier release
	code, body, _ := r.get("/fragments/desk")
	if code != 200 || !strings.Contains(body, "/sessions/"+m.ID) || !strings.Contains(body, "ScanSnap iX500 button") ||
		!strings.Contains(body, "/sessions/"+old.ID) || !strings.Contains(body, "ScanSnap button") {
		t.Fatalf("desk fragment %d:\n%s", code, body)
	}
}

func TestLabelsOfUnknownAndLegacyScanners(t *testing.T) {
	r := newRig(t)
	for in, want := range map[string]string{"feeder": "ScanSnap iX500", "scansnap": "ScanSnap", "epson": "Epson", "gone-scanner": "gone-scanner"} {
		if got := r.ws.scannerLabel(in); got != want {
			t.Errorf("scannerLabel(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"ui": "Desk", "button:feeder": "ScanSnap iX500 button", "button:gone": "gone button",
		"cli:glass": "Epson ET-2750 (command line)", "scansnap-button": "ScanSnap button", "epson": "Epson", "weird": "weird"} {
		if got := r.ws.sourceLabel(in); got != want {
			t.Errorf("sourceLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEveryScannerGetsCardTicketAndForm(t *testing.T) {
	r := newRig(t)
	_, body, _ := r.get("/")
	for _, want := range []string{`data-scanner="feeder" hx-post="/sessions"`, `data-scanner="glass" hx-post="/sessions"`,
		`<article class="card" data-tone="ok" data-scanner="feeder">`, `ScanSnap <em>iX500</em>`, `Press <kbd>Scan</kbd>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("desk lacks %q", want)
		}
	}
	_, _, h := r.send("POST", "/sessions", url.Values{"scanner": {"feeder"}}, nil)
	_, body, _ = r.get(h.Get("HX-Redirect"))
	for _, want := range []string{`<input type="hidden" name="scanner" value="feeder">`, `<input type="hidden" name="scanner" value="glass">`,
		`name="paper"`, `name="region"`, `Feed the stack`, `Scan the glass`, `Glass <small>Epson ET-2750</small>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("session page lacks %q", want)
		}
	}
	if !strings.Contains(body, `data-scanner="glass" x-show="shown" x-cloak`) || strings.Contains(body, `data-scanner="feeder" x-show="shown" x-cloak`) {
		t.Fatal("only the active scanner's form should be visible before Alpine starts")
	}

	_, _, h = r.send("POST", "/sessions", url.Values{"scanner": {"nope"}}, nil)
	if loc := h.Get("HX-Redirect"); strings.Contains(loc, "scanner=") {
		t.Fatalf("unknown scanner kept in %q", loc)
	}
	sid := strings.TrimPrefix(h.Get("HX-Redirect"), "/sessions/")
	if code, body, _ := r.send("POST", "/sessions/"+sid+"/scan", url.Values{"scanner": {"nope"}}, nil); code != 400 || !strings.Contains(body, "Pick a scanner") {
		t.Fatalf("unknown scanner scan: %d %s", code, body)
	}
	if code, body, _ := r.send("POST", "/sessions/"+sid+"/scan", url.Values{"scanner": {"glass"}, "color": {"pink"}, "dpi": {"300"}, "region": {"a4"}}, nil); code != 422 || !strings.Contains(body, "Invalid settings for Epson ET-2750") {
		t.Fatalf("bad settings: %d %s", code, body)
	}
}

func TestStatusJSONKeyedByScanner(t *testing.T) {
	r := newRig(t)
	_, body, _ := r.get("/status")
	var out struct {
		Scanners map[string]struct {
			Name, Kind, State string
			Device            map[string]any
		}
		Paperless map[string]any
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	f, g := out.Scanners["feeder"], out.Scanners["glass"]
	if f.Name != "ScanSnap iX500" || f.Kind != "feeder" || f.State != "Ready" || f.Device["state"] != "claimed" ||
		g.Kind != "flatbed" || g.State != "Ready" || out.Paperless == nil {
		t.Fatalf("status: %s", body)
	}
	if strings.Contains(body, "4242") {
		t.Fatal("password in /status")
	}
}

func TestNoNativeConfirmDialogs(t *testing.T) {
	// confirm() is unreliable in embedded/mobile browsers; destructive
	// actions use the tap-twice "confirm" Alpine component instead.
	b, _ := fs.ReadFile(templateFS, "templates/fragments.html")
	if strings.Contains(string(b), "hx-confirm") {
		t.Fatal("hx-confirm used")
	}
}

func TestDownloadSessionPDF(t *testing.T) {
	r := newRig(t)
	r.get("/")
	_, _, h := r.send("POST", "/sessions", nil, nil)
	sid := strings.TrimPrefix(h.Get("HX-Redirect"), "/sessions/")
	if code, _, _ := r.get("/sessions/" + sid + "/pdf"); code != 422 {
		t.Fatalf("empty session PDF: %d", code)
	}
	r.store.AddPage(sid, venstest.PageJPEG(0, 0), "scansnap", 0, 0)
	r.store.SetTitle(sid, "Facture été / EDF")
	_, page, _ := r.get("/sessions/" + sid + "/pages")
	if !strings.Contains(page, `href="/sessions/`+sid+`/pdf"`) {
		t.Fatal("no download link")
	}
	code, body, hdr := r.get("/sessions/" + sid + "/pdf")
	if code != 200 || hdr.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(body, "%PDF-") {
		t.Fatalf("pdf %d %s", code, hdr.Get("Content-Type"))
	}
	if cd := hdr.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "Facture") || strings.Contains(cd, "/ EDF") {
		t.Fatalf("disposition %q", cd)
	}
	if _, err := r.store.Get(sid); err != nil {
		t.Fatal("download must not consume the session")
	}
}

func TestReleaseAndTakeBackFromTheDesk(t *testing.T) {
	r := newRig(t)
	_, body, _ := r.get("/")
	if !strings.Contains(body, `hx-post="/scanners/feeder/release"`) || strings.Contains(body, `/scanners/glass/release`) {
		t.Fatal("only the ScanSnap card offers Release")
	}
	code, body, _ := r.send("POST", "/scanners/feeder/release", nil, nil)
	if code != 200 || !strings.Contains(body, ">Released<") || !strings.Contains(body, `hx-post="/scanners/feeder/take-back"`) {
		t.Fatalf("release %d:\n%s", code, body)
	}
	if _, held := r.ss.Holder(); held {
		t.Fatal("still held")
	}
	_, _, h := r.send("POST", "/sessions", nil, nil)
	sid := strings.TrimPrefix(h.Get("HX-Redirect"), "/sessions/")
	if code, body, _ := r.send("POST", "/sessions/"+sid+"/scan", url.Values{"scanner": {"feeder"}, "color": {"auto"}, "dpi": {"300"}, "paper": {"auto"}}, nil); code != 409 || !strings.Contains(body, "is released") {
		t.Fatalf("scan while released: %d %s", code, body)
	}
	if code, _, _ := r.send("POST", "/scanners/glass/release", nil, nil); code != 404 {
		t.Fatalf("eSCL release: %d", code)
	}
	if code, body, _ := r.send("POST", "/scanners/feeder/take-back", nil, nil); code != 200 || !strings.Contains(body, `/scanners/feeder/release`) {
		t.Fatalf("take back %d:\n%s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, held := r.ss.Holder(); held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not reclaimed after take back")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
