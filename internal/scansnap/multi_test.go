package scansnap

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guigui42/scan-hub/internal/vens"
	"github.com/guigui42/scan-hub/internal/vens/venstest"
)

// loopbackOrSkip skips when ip (another 127/8 address) cannot be bound,
// e.g. on macOS without lo0 aliases.
func loopbackOrSkip(t *testing.T, ip string) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Skipf("cannot bind %s (needs Linux or a lo0 alias): %v", ip, err)
	}
	c.Close()
}

func buttonPacket(seq byte) []byte {
	p := make([]byte, 48)
	binary.BigEndian.PutUint32(p[0:], 48)
	copy(p[4:], vens.Magic[:])
	binary.BigEndian.PutUint32(p[8:], 1)
	p[16] = seq
	return p
}

func sendFrom(t *testing.T, from string, port int, pkt []byte) {
	t.Helper()
	c, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP(from)}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(pkt); err != nil {
		t.Fatal(err)
	}
}

func newHub(t *testing.T) *NotifyHub {
	t.Helper()
	h := NewNotifyHub(0, slog.New(slog.DiscardHandler))
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func TestHubRoutesBySourceIP(t *testing.T) {
	loopbackOrSkip(t, "127.0.0.2")
	h := newHub(t)
	if h.Port() == 0 {
		t.Fatal("port 0 must be resolved to the bound port")
	}
	a, unsubA, err := h.Subscribe("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer unsubA()
	b, unsubB, err := h.Subscribe("127.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Subscribe("127.0.0.2"); !errors.Is(err, ErrSubscribed) {
		t.Fatalf("duplicate subscribe: %v", err)
	}
	if _, _, err := h.Subscribe("scanner.example.com"); err == nil {
		t.Fatal("hostnames must be rejected")
	}

	sendFrom(t, "127.0.0.2", h.Port(), buttonPacket(7))
	select {
	case ev := <-b:
		if ev.Announce || ev.Seq != 7<<24 {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not routed to 127.0.0.2")
	}
	select {
	case ev := <-a:
		t.Fatalf("127.0.0.1 got another scanner's event %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	sendFrom(t, "127.0.0.2", h.Port(), []byte("noise"))
	unsubB()
	unsubB() // idempotent
	if _, ok := <-b; ok {
		t.Fatal("channel must be closed after unsubscribe")
	}
	// Unknown source now: dropped without panicking.
	sendFrom(t, "127.0.0.2", h.Port(), buttonPacket(8))

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-a; ok {
		t.Fatal("Close must close remaining subscriptions")
	}
	unsubA() // after Close: no panic
	if _, _, err := h.Subscribe("127.0.0.3"); !errors.Is(err, ErrHubClosed) {
		t.Fatalf("subscribe after close: %v", err)
	}
}

func TestHubUnsubscribeRacesDispatch(t *testing.T) {
	h := newHub(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: h.Port()})
		if err != nil {
			return
		}
		defer c.Close()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c.Write(buttonPacket(byte(i)))
		}
	}()
	for range 200 {
		ch, unsub, err := h.Subscribe("127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for range ch {
			}
		}()
		unsub()
	}
	close(stop)
	wg.Wait()
}

func TestHubPortConflictFailsImmediately(t *testing.T) {
	h := newHub(t)
	if err := NewNotifyHub(h.Port(), nil).Listen(); err == nil {
		t.Fatal("second bind of the same port must fail")
	}
	idle := NewNotifyHub(0, nil)
	if _, _, err := idle.Subscribe("127.0.0.1"); !errors.Is(err, ErrNotListening) {
		t.Fatalf("subscribe before Listen: %v", err)
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRejectsConflictingNotifyPort(t *testing.T) {
	f := startFake(t, venstest.Options{})
	h := newHub(t)
	cfg := testConfig(t, f, filepath.Join(t.TempDir(), "j.json"))
	cfg.Hub = h
	cfg.NotifyPort = h.Port() + 1
	if err := New(cfg).Run(context.Background()); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("want a port conflict error, got %v", err)
	}
}

func TestManagerRunsOnce(t *testing.T) {
	f := startFake(t, venstest.Options{})
	r := run(t, testConfig(t, f, filepath.Join(t.TempDir(), "j.json")))
	waitState(t, r.m, StateClaimed)
	if err := r.m.Run(context.Background()); err == nil {
		t.Fatal("second Run must fail")
	}
	r.stop()
	if _, ok := <-r.m.Buttons(); ok {
		t.Fatal("Buttons must be closed after Run returns")
	}
}

// Two ScanSnaps share the button port and the discovery reply port.
func TestTwoScanSnapsShareHubAndDiscovery(t *testing.T) {
	loopbackOrSkip(t, "127.0.0.2")
	loopbackOrSkip(t, "127.0.0.3")
	fa := startFake(t, venstest.Options{IP: "127.0.0.2", Password: "1111", HeartbeatACK: true})
	fb := startFake(t, venstest.Options{IP: "127.0.0.3", Password: "2222", HeartbeatACK: true})
	h := newHub(t)
	reply := freeUDPPort(t)
	dir := t.TempDir()
	cfg := func(f *venstest.Scanner, id, pw string) Config {
		c := testConfig(t, f, JournalPath(dir, id))
		c.Password, c.Hub, c.NotifyPort, c.DiscoveryReplyPort = pw, h, 0, reply
		return c
	}
	ra := run(t, cfg(fa, "a", "1111"))
	rb := run(t, cfg(fb, "b", "2222"))
	waitState(t, ra.m, StateClaimed)
	waitState(t, rb.m, StateClaimed)

	for _, tc := range []struct {
		press, other *running
		f            *venstest.Scanner
	}{{ra, rb, fa}, {rb, ra, fb}} {
		if !tc.f.PressButton() {
			t.Fatal("fake has no session")
		}
		select {
		case <-tc.press.m.Buttons():
		case <-time.After(2 * time.Second):
			t.Fatalf("no button event for %s", tc.f.IP())
		}
		select {
		case ev := <-tc.other.m.Buttons():
			t.Fatalf("event of %s reached the other manager: %+v", tc.f.IP(), ev)
		case <-time.After(150 * time.Millisecond):
		}
	}

	for _, c := range []struct {
		id string
		f  *venstest.Scanner
	}{{"a", fa}, {"b", fb}} {
		js, _ := journal{path: JournalPath(dir, c.id)}.load()
		if !js.Dirty || js.ScannerIP != c.f.IP() {
			t.Fatalf("journal %s: %+v", c.id, js)
		}
	}
	ra.stop()
	rb.stop()
	noForbidden(t, fa)
	noForbidden(t, fb)
}

func TestDirtyJournalOfAnotherScannerIsNotReused(t *testing.T) {
	f := startFake(t, venstest.Options{})
	jp := filepath.Join(t.TempDir(), "j.json")
	stale := vens.NewToken()
	if err := (journal{path: jp, ip: "192.168.1.51"}).begin(stale); err != nil {
		t.Fatal(err)
	}
	r := run(t, testConfig(t, f, jp))
	st := waitState(t, r.m, StateNotConnected)
	time.Sleep(200 * time.Millisecond)
	if f.Stats().Reserves != 0 || !strings.Contains(r.m.Status().Detail, "192.168.1.51") {
		t.Fatalf("token of another scanner was used: reserves=%d status=%+v", f.Stats().Reserves, st)
	}
}

func writeJournal(t *testing.T, path string, st journalState) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareJournals(t *testing.T) {
	dirty := journalState{Token: "0102030405060000", Dirty: true}
	clean := journalState{Token: "0102030405060000"}
	one := map[string]string{"feeder": "192.168.1.50"}
	two := map[string]string{"feeder": "192.168.1.50", "other": "192.168.1.51"}

	t.Run("legacy moves to the only ScanSnap", func(t *testing.T) {
		dir := t.TempDir()
		writeJournal(t, filepath.Join(dir, "journal.json"), dirty)
		if err := PrepareJournals(dir, one); err != nil {
			t.Fatal(err)
		}
		st, err := journal{path: JournalPath(dir, "feeder")}.load()
		if err != nil || !st.Dirty || st.Token != dirty.Token || st.ScannerIP != "192.168.1.50" {
			t.Fatalf("migrated %+v, %v", st, err)
		}
		// Bound to its scanner: a changed IP can no longer adopt its token.
		if err := PrepareJournals(dir, map[string]string{"feeder": "192.168.1.99"}); err == nil {
			t.Fatal("migrated dirty journal accepted for another IP")
		}
		if _, err := os.Stat(filepath.Join(dir, "journal.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legacy journal left behind")
		}
	})
	t.Run("legacy never overwrites", func(t *testing.T) {
		dir := t.TempDir()
		writeJournal(t, filepath.Join(dir, "journal.json"), dirty)
		writeJournal(t, JournalPath(dir, "feeder"), clean)
		if err := PrepareJournals(dir, one); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("dirty legacy with several ScanSnaps", func(t *testing.T) {
		dir := t.TempDir()
		writeJournal(t, filepath.Join(dir, "journal.json"), dirty)
		if err := PrepareJournals(dir, two); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("clean legacy with several ScanSnaps", func(t *testing.T) {
		dir := t.TempDir()
		writeJournal(t, filepath.Join(dir, "journal.json"), clean)
		if err := PrepareJournals(dir, two); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "journal.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("clean legacy journal kept")
		}
		// The managers then write their own journals; going back to one
		// ScanSnap must still start.
		writeJournal(t, JournalPath(dir, "feeder"), clean)
		writeJournal(t, JournalPath(dir, "other"), clean)
		if err := PrepareJournals(dir, one); err != nil {
			t.Fatal("two-to-one ScanSnaps:", err)
		}
	})
	t.Run("malformed legacy with one ScanSnap", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "journal.json"), []byte("{"), 0o600)
		if err := PrepareJournals(dir, one); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("malformed legacy with several ScanSnaps", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "journal.json"), []byte("{"), 0o600)
		if err := PrepareJournals(dir, two); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("dirty journal recorded for another IP", func(t *testing.T) {
		dir := t.TempDir()
		st := dirty
		st.ScannerIP = "192.168.1.99"
		writeJournal(t, JournalPath(dir, "feeder"), st)
		if err := PrepareJournals(dir, one); err == nil {
			t.Fatal("want an error")
		}
		st.Dirty = false
		writeJournal(t, JournalPath(dir, "feeder"), st)
		if err := PrepareJournals(dir, one); err != nil {
			t.Fatal("clean journal of another IP must pass:", err)
		}
	})
	t.Run("orphan dirty journal (renamed or removed id)", func(t *testing.T) {
		dir := t.TempDir()
		writeJournal(t, JournalPath(dir, "old-name"), dirty)
		if err := PrepareJournals(dir, one); err == nil || !strings.Contains(err.Error(), "old-name") {
			t.Fatalf("want an error naming old-name, got %v", err)
		}
		writeJournal(t, JournalPath(dir, "old-name"), clean)
		if err := PrepareJournals(dir, one); err != nil {
			t.Fatal("clean orphan must pass:", err)
		}
	})
	t.Run("missing dir", func(t *testing.T) {
		if err := PrepareJournals(filepath.Join(t.TempDir(), "nope"), one); err != nil {
			t.Fatal(err)
		}
	})
}

func announceHub(t *testing.T) *NotifyHub {
	t.Helper()
	h := NewNotifyHub(0, slog.New(slog.DiscardHandler))
	h.ListenAnnouncements(0)
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	if h.AnnouncePort() == 0 {
		t.Fatal("announcement port not bound")
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// Chute closed long enough to lose contact, then opened: the power-on
// announcement reconnects at once instead of after the retry backoff.
func TestAnnouncementReconnectsAnOfflineScanner(t *testing.T) {
	f := startFake(t, venstest.Options{})
	h := announceHub(t)
	cfg := testConfig(t, f, filepath.Join(t.TempDir(), "j.json"))
	cfg.Hub, cfg.NotifyPort = h, 0
	cfg.RetryMin, cfg.RetryMax = time.Minute, time.Minute // only an announcement can bring it back in time
	r := run(t, cfg)
	waitState(t, r.m, StateClaimed)

	f.PowerOff()
	st := waitState(t, r.m, StateConnecting)
	if st.Detail != "lost contact with scanner" {
		t.Fatalf("status %+v", st)
	}
	time.Sleep(200 * time.Millisecond)
	f.PowerOn(h.AnnouncePort())
	start := time.Now()
	waitState(t, r.m, StateClaimed)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("reclaimed after %v", d)
	}
	if !f.PressButton() {
		t.Fatal("fake has no session after reclaim")
	}
	select {
	case <-r.m.Buttons():
	case <-time.After(2 * time.Second):
		t.Fatal("button not delivered after reclaim")
	}
	noForbidden(t, f)
}

// Chute closed and reopened between two polls: the manager still thinks it
// holds the scanner, so the announcement makes it reclaim (once per burst).
func TestAnnouncementWhileClaimedReclaimsOnce(t *testing.T) {
	f := startFake(t, venstest.Options{})
	h := announceHub(t)
	cfg := testConfig(t, f, filepath.Join(t.TempDir(), "j.json"))
	cfg.Hub, cfg.NotifyPort = h, 0
	cfg.StatusInterval = time.Hour // polling would not notice
	r := run(t, cfg)
	waitState(t, r.m, StateClaimed)
	before, _ := f.Holder()

	f.PowerOff()
	f.PowerOn(h.AnnouncePort())
	deadline := time.Now().Add(5 * time.Second)
	for f.Stats().Reserves < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("no reclaim; reserves %d", f.Stats().Reserves)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitState(t, r.m, StateClaimed)
	time.Sleep(300 * time.Millisecond)
	if n := f.Stats().Reserves; n != 2 {
		t.Fatalf("burst not debounced: %d reserves", n)
	}
	if after, held := f.Holder(); !held || after != before {
		t.Fatalf("reclaim should reuse the journal token: %x -> %x (held %v)", before, after, held)
	}
	if !f.PressButton() {
		t.Fatal("fake has no session after reclaim")
	}
	select {
	case <-r.m.Buttons():
	case <-time.After(2 * time.Second):
		t.Fatal("button not delivered after reclaim")
	}
	noForbidden(t, f)
}

func TestAnnouncementsOfOtherScannersIgnored(t *testing.T) {
	loopbackOrSkip(t, "127.0.0.2")
	h := announceHub(t)
	ch, unsub, err := h.Subscribe("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	other, err := venstest.Start(venstest.Options{Password: "1", IP: "127.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.PowerOn(h.AnnouncePort())
	sendFrom(t, "127.0.0.1", h.AnnouncePort(), []byte("not vens"))
	select {
	case ev := <-ch:
		t.Fatalf("got %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}
