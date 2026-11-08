package scansnapdrv

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guigui42/scan-hub/internal/scanner"
	"github.com/guigui42/scan-hub/internal/scannerconfig"
	"github.com/guigui42/scan-hub/internal/scansnap"
	"github.com/guigui42/scan-hub/internal/vens"
	"github.com/guigui42/scan-hub/internal/vens/venstest"
)

func TestMapErr(t *testing.T) {
	for in, want := range map[error]error{
		scansnap.ErrBusy:                             scanner.ErrBusy,
		scansnap.ErrNotReady:                         scanner.ErrNotReady,
		&vens.ScanError{Kind: vens.ScanErrNoPaper}:   scanner.ErrNoPaper,
		&vens.ScanError{Kind: vens.ScanErrPaperJam}:  scanner.ErrJam,
		&vens.ScanError{Kind: vens.ScanErrCoverOpen}: scanner.ErrCoverOpen,
		&vens.ScanError{Kind: vens.ScanErrMultiFeed}: scanner.ErrMultiFeed,
	} {
		if got := mapErr(in); !errors.Is(got, want) || !errors.Is(got, in) {
			t.Errorf("mapErr(%v) = %v, want %v", in, got, want)
		}
	}
	if mapErr(nil) != nil {
		t.Fatal("nil")
	}
}

// End to end through the scanner list: the driver's shared hub, a claim,
// a button press forwarded by Run, a scan, and Buttons closed after Run.
func TestDeviceFromListRunsAndForwardsButton(t *testing.T) {
	f, err := venstest.Start(venstest.Options{Password: "4242", HeartbeatACK: true})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dir := t.TempDir()
	drv := New(Options{
		DataDir: dir, Logger: slog.New(slog.DiscardHandler), AnnouncePort: -1,
		LookupEnv: func(k string) (string, bool) { return "4242", k == "PW" },
		Tune: func(id string, c *scansnap.Config) {
			c.ControlPort, c.DataPort, c.DiscoveryPort = f.ControlPort(), f.DataPort(), f.DiscoveryPort()
			c.HeartbeatInterval, c.StatusInterval, c.RetryMin = 50*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
		},
	})
	drv.opts.NotifyPort = 0 // a free port instead of 55265
	devs, err := scannerconfig.Parse([]byte("scanners:\n  - id: feeder\n    type: scansnap\n    ip: 127.0.0.1\n    password_env: PW\n"),
		map[string]scannerconfig.Driver{Type: drv})
	if err != nil {
		t.Fatal(err)
	}
	if err := drv.PrepareJournals(); err != nil {
		t.Fatal(err)
	}
	if err := drv.Listen(); err != nil {
		t.Fatal(err)
	}
	dev := devs[0].(*Device)
	if err := dev.Ready(); !errors.Is(err, scanner.ErrNotReady) || !strings.HasPrefix(err.Error(), "feeder is not connected yet") {
		t.Fatalf("before Run: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dev.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for dev.Ready() != nil {
		if time.Now().After(deadline) {
			t.Fatalf("not ready: %v", dev.Ready())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := dev.Status(ctx); st.Label != "Ready" || st.Tone != scanner.ToneOK {
		t.Fatalf("status %+v", st)
	}
	if !f.PressButton() {
		t.Fatal("fake has no session")
	}
	select {
	case <-dev.Buttons():
	case <-time.After(2 * time.Second):
		t.Fatal("no button event")
	}
	f.LoadPaper(1)
	s, err := dev.ParseForm(func(k string) string {
		return map[string]string{"color": "auto", "dpi": "300", "paper": "auto"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	var pages int
	if err := dev.Scan(ctx, s, func(scanner.Page) error { pages++; return nil }); err != nil || pages != 1 {
		t.Fatalf("simplex scan: %v pages=%d", err, pages)
	}
	if err := dev.Scan(ctx, s, func(scanner.Page) error { return nil }); !errors.Is(err, scanner.ErrNoPaper) {
		t.Fatalf("empty feeder: %v", err)
	}
	if err := dev.Run(ctx); err == nil {
		t.Fatal("second Run must fail")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := <-dev.Buttons(); ok {
		t.Fatal("Buttons must close when Run returns")
	}
	if err := drv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scansnap", "feeder", "journal.json")); err != nil {
		t.Fatal("journal not under the scanner id:", err)
	}
	if fb := f.Stats().Forbidden; len(fb) > 0 {
		t.Fatalf("forbidden commands: %v", fb)
	}
}
