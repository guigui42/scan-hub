package scansnap

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/guigui42/scan-hub/internal/vens"
	"github.com/guigui42/scan-hub/internal/vens/venstest"
)

const testPassword = "4242"

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func startFake(t *testing.T, opts venstest.Options) *venstest.Scanner {
	t.Helper()
	if opts.Password == "" {
		opts.Password = testPassword
	}
	f, err := venstest.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func testConfig(t *testing.T, f *venstest.Scanner, journalPath string) Config {
	return Config{
		ScannerIP:          f.IP(),
		Password:           testPassword,
		JournalPath:        journalPath,
		ControlPort:        f.ControlPort(),
		DataPort:           f.DataPort(),
		DiscoveryPort:      f.DiscoveryPort(),
		DiscoveryReplyPort: freeUDPPort(t),
		NotifyPort:         freeUDPPort(t),
		HeartbeatInterval:  50 * time.Millisecond,
		StatusInterval:     100 * time.Millisecond,
		RetryMin:           50 * time.Millisecond,
		RetryMax:           200 * time.Millisecond,
		BadPasswordRetry:   time.Hour,
		ScanGrace:          time.Second,
		Logger:             slog.New(slog.DiscardHandler),
	}
}

type running struct {
	m      *Manager
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func run(t *testing.T, cfg Config) *running {
	t.Helper()
	m := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{m: m, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- m.Run(ctx) }()
	t.Cleanup(r.stop)
	return r
}

func (r *running) stop() {
	r.once.Do(func() {
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			panic("manager did not stop")
		}
	})
}

func waitState(t *testing.T, m *Manager, want State) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := m.Status(); st.State == want {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state %q not reached; last %+v", want, m.Status())
	return Status{}
}

func noForbidden(t *testing.T, f *venstest.Scanner) {
	t.Helper()
	if fb := f.Stats().Forbidden; len(fb) > 0 {
		t.Fatalf("forbidden commands reached the scanner: %v", fb)
	}
}

func TestClaimButtonScanRelease(t *testing.T) {
	f := startFake(t, venstest.Options{EndOfBatchQuirk: true, ChunkSize: 700})
	jp := filepath.Join(t.TempDir(), "journal.json")
	r := run(t, testConfig(t, f, jp))
	st := waitState(t, r.m, StateClaimed)
	if st.DeviceName != "FUJITSU ScanSnap iX500" || st.Firmware != "0U00" || st.WifiSignal != 3 {
		t.Fatalf("status %+v", st)
	}
	js, _ := journal{path: jp}.load()
	if !js.Dirty {
		t.Fatal("journal should be dirty while claimed")
	}

	if !f.PressButton() {
		t.Fatal("fake has no session")
	}
	select {
	case ev := <-r.m.Buttons():
		if ev.Seq == 0 {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no button event")
	}
	select {
	case ev := <-r.m.Buttons():
		t.Fatalf("repeat datagrams not debounced: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}

	f.LoadPaper(2)
	var pages []Page
	cfg := vens.DefaultScanConfig()
	cfg.Duplex = true
	res := r.m.Scan(context.Background(), ScanRequest{Config: cfg, OnPage: func(p Page) error {
		pages = append(pages, p)
		return nil
	}})
	if res.Err != nil || res.Pages != 4 || len(pages) != 4 {
		t.Fatalf("scan: %+v pages=%d", res, len(pages))
	}
	for i, p := range pages {
		if p.Sheet != i/2 || p.Side != i%2 || len(p.Data) == 0 {
			t.Fatalf("page %d: sheet=%d side=%d bytes=%d", i, p.Sheet, p.Side, len(p.Data))
		}
	}
	if f.Stats().EndScans != 1 {
		t.Fatalf("D6 not sent once: %d", f.Stats().EndScans)
	}

	r.stop()
	if _, held := f.Holder(); held {
		t.Fatal("scanner still held after shutdown")
	}
	js, _ = journal{path: jp}.load()
	if js.Dirty {
		t.Fatal("journal dirty after clean shutdown")
	}
	for _, a := range f.Stats().ReleaseActions {
		if a != 0 {
			t.Fatalf("non-normal RELEASE action %d", a)
		}
	}
	if r.m.Status().State != StateStopped {
		t.Fatalf("state %s", r.m.Status().State)
	}
	noForbidden(t, f)
}

func TestNoPaper(t *testing.T) {
	f := startFake(t, venstest.Options{})
	r := run(t, testConfig(t, f, filepath.Join(t.TempDir(), "j.json")))
	waitState(t, r.m, StateClaimed)
	res := r.m.Scan(context.Background(), ScanRequest{Config: vens.DefaultScanConfig(), OnPage: func(Page) error { return nil }})
	var se *vens.ScanError
	if !errors.As(res.Err, &se) || se.Kind != vens.ScanErrNoPaper {
		t.Fatalf("want no-paper error, got %v", res.Err)
	}
	if n := f.Stats().EndScans; n != 1 {
		t.Fatalf("D6 after failed scan start: got %d, want 1", n)
	}
	waitState(t, r.m, StateClaimed)
	noForbidden(t, f)
}

func TestCancelDuringWaitIsPrompt(t *testing.T) {
	f := startFake(t, venstest.Options{WaitDelay: 2 * time.Second})
	r := run(t, testConfig(t, f, filepath.Join(t.TempDir(), "j.json")))
	waitState(t, r.m, StateClaimed)
	f.LoadPaper(1)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := r.m.Scan(ctx, ScanRequest{Config: vens.DefaultScanConfig(), OnPage: func(Page) error { return nil }})
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", res.Err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancel took %s", d)
	}
	waitState(t, r.m, StateClaimed)
	stopStart := time.Now()
	r.stop()
	if d := time.Since(stopStart); d > 2*time.Second {
		t.Fatalf("shutdown took %s", d)
	}
}

func TestBusyWhileScanning(t *testing.T) {
	f := startFake(t, venstest.Options{})
	r := run(t, testConfig(t, f, filepath.Join(t.TempDir(), "j.json")))
	waitState(t, r.m, StateClaimed)
	f.LoadPaper(1)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.m.Scan(context.Background(), ScanRequest{Config: vens.DefaultScanConfig(), OnPage: func(Page) error {
			<-release
			return nil
		}})
	}()
	waitState(t, r.m, StateScanning)
	if res := r.m.Scan(context.Background(), ScanRequest{OnPage: func(Page) error { return nil }}); !errors.Is(res.Err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", res.Err)
	}
	close(release)
	wg.Wait()
	waitState(t, r.m, StateClaimed)
}

func TestBadPasswordDoesNotHammer(t *testing.T) {
	f := startFake(t, venstest.Options{Password: "9999"})
	jp := filepath.Join(t.TempDir(), "j.json")
	r := run(t, testConfig(t, f, jp))
	st := waitState(t, r.m, StateBadPassword)
	if st.Detail == "" {
		t.Fatal("no detail")
	}
	time.Sleep(300 * time.Millisecond)
	if n := f.Stats().Reserves; n != 1 {
		t.Fatalf("RESERVE sent %d times", n)
	}
	js, _ := journal{path: jp}.load()
	if js.Dirty {
		t.Fatal("journal dirty after rejected RESERVE")
	}
}

func TestRecoverWithJournalToken(t *testing.T) {
	f := startFake(t, venstest.Options{HeartbeatTimeout: 2 * time.Second})
	jp := filepath.Join(t.TempDir(), "j.json")
	// A previous scan-hub process crashed while holding the scanner.
	stale := vens.NewToken()
	if err := (journal{path: jp}).begin(stale); err != nil {
		t.Fatal(err)
	}
	f.Occupy(stale, net.IPv4(127, 0, 0, 1), 2*time.Second)

	r := run(t, testConfig(t, f, jp))
	waitState(t, r.m, StateClaimed)
	holder, ok := f.Holder()
	if !ok || holder != stale {
		t.Fatalf("holder %x, want adopted %x", holder, stale)
	}
	if codes := f.Stats().ReserveCodes; len(codes) != 1 || codes[0] != 0 {
		t.Fatalf("reserve codes %v", codes)
	}
	noForbidden(t, f)
}

func TestForeignHolderBacksOffThenClaims(t *testing.T) {
	f := startFake(t, venstest.Options{HeartbeatTimeout: time.Second})
	jp := filepath.Join(t.TempDir(), "j.json")
	other := vens.NewToken()
	f.Occupy(other, net.IPv4(127, 0, 0, 1), 700*time.Millisecond)

	r := run(t, testConfig(t, f, jp))
	st := waitState(t, r.m, StateHeldByOther)
	if st.Holder != "127.0.0.1" {
		t.Fatalf("holder %q", st.Holder)
	}
	js, _ := journal{path: jp}.load()
	if js.Dirty {
		t.Fatal("journal must not stay dirty for a token we never held")
	}
	waitState(t, r.m, StateClaimed)
	holder, _ := f.Holder()
	if holder == other {
		t.Fatal("claimed with the foreign token")
	}
	for _, a := range f.Stats().ReleaseActions {
		if a != 0 {
			t.Fatal("ENFORCE used")
		}
	}
	if f.Stats().Releases != 0 {
		t.Fatal("released a foreign session")
	}
	noForbidden(t, f)
}

func TestLostContactThenReclaimSameToken(t *testing.T) {
	f := startFake(t, venstest.Options{})
	jp := filepath.Join(t.TempDir(), "j.json")
	cfg := testConfig(t, f, jp)
	r := run(t, cfg)
	waitState(t, r.m, StateClaimed)
	first, _ := f.Holder()
	f.Close()
	waitState(t, r.m, StateConnecting)
	js, _ := journal{path: jp}.load()
	if !js.Dirty || js.Token != hex.EncodeToString(first[:]) {
		t.Fatalf("journal should keep the held token dirty: %+v", js)
	}
}

func TestJournalNeverOverwritesDirtyToken(t *testing.T) {
	j := journal{path: filepath.Join(t.TempDir(), "j.json")}
	a, b := vens.NewToken(), vens.NewToken()
	if err := j.begin(a); err != nil {
		t.Fatal(err)
	}
	if err := j.begin(b); !errors.Is(err, ErrDirtyJournal) {
		t.Fatalf("want ErrDirtyJournal, got %v", err)
	}
	if err := j.begin(a); err != nil {
		t.Fatal("same token must be allowed:", err)
	}
	if err := j.clean(); err != nil {
		t.Fatal(err)
	}
	if err := j.begin(b); err != nil {
		t.Fatal(err)
	}
}

func TestParseButton(t *testing.T) {
	p := make([]byte, 48)
	copy(p[4:], vens.Magic[:])
	p[11] = 1
	p[16] = 3
	if seq, ok := parseButton(p); !ok || seq != 0x03000000 {
		t.Fatalf("seq %x ok %v", seq, ok)
	}
	if _, ok := parseButton(p[:47]); ok {
		t.Fatal("short packet accepted")
	}
	p[11] = 2
	if _, ok := parseButton(p); ok {
		t.Fatal("wrong type accepted")
	}
}

// After a power cycle another client may hold the scanner: RESERVE with the
// journal token gets -4 and the RELEASE with it is refused. The manager must
// not loop on that token: it reports the holder, forgets the token, and
// claims with a fresh one once the scanner is free.
func TestRefusedReleaseOfJournalTokenFallsBackToHeldByOther(t *testing.T) {
	f := startFake(t, venstest.Options{HeartbeatTimeout: time.Second, CloseOnForeignRelease: true})
	jp := filepath.Join(t.TempDir(), "j.json")
	mine := vens.NewToken()
	if err := (journal{path: jp}).begin(mine); err != nil {
		t.Fatal(err)
	}
	other := vens.NewToken()
	f.Occupy(other, net.IPv4(127, 0, 0, 1), 800*time.Millisecond)

	r := run(t, testConfig(t, f, jp))
	st := waitState(t, r.m, StateHeldByOther)
	if st.Holder != "127.0.0.1" {
		t.Fatalf("status %+v", st)
	}
	if js, _ := (journal{path: jp}).load(); js.Dirty {
		t.Fatal("journal token kept after the scanner refused its release")
	}
	waitState(t, r.m, StateClaimed)
	if holder, _ := f.Holder(); holder == mine || holder == other {
		t.Fatalf("expected a fresh token, holder %x", holder)
	}
	noForbidden(t, f)
}

func TestReleaseAndTakeBackSurviveRestart(t *testing.T) {
	f := startFake(t, venstest.Options{})
	jp := filepath.Join(t.TempDir(), "j.json")
	r := run(t, testConfig(t, f, jp))
	waitState(t, r.m, StateClaimed)

	ctx := context.Background()
	if err := r.m.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if st := r.m.Status(); st.State != StateReleased {
		t.Fatalf("state %s", st.State)
	}
	if _, held := f.Holder(); held {
		t.Fatal("scanner still held after Release")
	}
	if js, _ := (journal{path: jp}).load(); js.Dirty || !js.Released {
		t.Fatalf("journal %+v", js)
	}
	if err := r.m.Release(ctx); err != nil {
		t.Fatal("second Release must be a no-op:", err)
	}
	reserves := f.Stats().Reserves
	time.Sleep(300 * time.Millisecond) // well past RetryMin: no reclaim
	if f.Stats().Reserves != reserves {
		t.Fatal("reclaimed while released")
	}
	r.stop()

	// A restart stays released.
	r2 := run(t, testConfig(t, f, jp))
	waitState(t, r2.m, StateReleased)
	time.Sleep(300 * time.Millisecond)
	if f.Stats().Reserves != reserves {
		t.Fatal("claimed after restart while released")
	}
	if err := r2.m.TakeBack(ctx); err != nil {
		t.Fatal(err)
	}
	waitState(t, r2.m, StateClaimed)
	if js, _ := (journal{path: jp}).load(); js.Released || !js.Dirty {
		t.Fatalf("journal after take back %+v", js)
	}
	for _, a := range f.Stats().ReleaseActions {
		if a != 0 {
			t.Fatalf("non-normal RELEASE action %d", a)
		}
	}
	noForbidden(t, f)
}

func TestReleaseRefusedWhileScanning(t *testing.T) {
	f := startFake(t, venstest.Options{WaitDelay: 500 * time.Millisecond})
	r := run(t, testConfig(t, f, filepath.Join(t.TempDir(), "j.json")))
	waitState(t, r.m, StateClaimed)
	f.LoadPaper(1)
	done := make(chan ScanResult, 1)
	go func() {
		done <- r.m.Scan(context.Background(), ScanRequest{Config: vens.DefaultScanConfig(), OnPage: func(Page) error { return nil }})
	}()
	waitState(t, r.m, StateScanning)
	if err := r.m.Release(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("release during scan: %v", err)
	}
	<-done
	if err := r.m.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTakeBackWhileClaimedIsANoop(t *testing.T) {
	f := startFake(t, venstest.Options{})
	r := run(t, testConfig(t, f, filepath.Join(t.TempDir(), "j.json")))
	waitState(t, r.m, StateClaimed)
	reserves := f.Stats().Reserves
	if err := r.m.TakeBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if st := r.m.Status().State; st != StateClaimed || f.Stats().Reserves != reserves {
		t.Fatalf("take back while claimed reclaimed: state %s, reserves %d -> %d", st, reserves, f.Stats().Reserves)
	}
}

func TestReleaseRetriesRememberingIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := startFake(t, venstest.Options{})
	dir := t.TempDir()
	jp := filepath.Join(dir, "j.json")
	r := run(t, testConfig(t, f, jp))
	waitState(t, r.m, StateClaimed)

	os.Chmod(dir, 0o500) // journal writes fail
	err := r.m.Release(context.Background())
	os.Chmod(dir, 0o700)
	if err == nil || r.m.Status().State != StateReleased {
		t.Fatalf("want a released scanner and a write error, got %v / %s", err, r.m.Status().State)
	}
	if err := r.m.Release(context.Background()); err != nil {
		t.Fatal("retry:", err)
	}
	if js, _ := (journal{path: jp}).load(); !js.Released {
		t.Fatalf("retry did not remember the release: %+v", js)
	}
}
