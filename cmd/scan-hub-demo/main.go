// Command scan-hub-demo runs the real web UI against fake devices (a fake
// ScanSnap, Epson and Paperless on loopback) for UI work and browser tests.
// It never talks to real hardware.
//
//	go run ./cmd/scan-hub-demo -addr 127.0.0.1:8098
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/guigui42/scan-hub/internal/config"
	"github.com/guigui42/scan-hub/internal/escl"
	"github.com/guigui42/scan-hub/internal/escl/escltest"
	"github.com/guigui42/scan-hub/internal/paperless"
	"github.com/guigui42/scan-hub/internal/paperless/paperlesstest"
	"github.com/guigui42/scan-hub/internal/scansnap"
	"github.com/guigui42/scan-hub/internal/session"
	"github.com/guigui42/scan-hub/internal/vens"
	"github.com/guigui42/scan-hub/internal/vens/venstest"
	"github.com/guigui42/scan-hub/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8098", "listen address")
	dir := flag.String("data", "", "data directory (default: a temporary one)")
	paper := flag.Int("paper", 3, "sheets in the fake ScanSnap feeder, refilled after each scan")
	flag.Parse()
	if err := run(*addr, *dir, *paper); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func freeUDP() int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		panic(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func run(addr, dir string, paper int) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	if dir == "" {
		d, err := os.MkdirTemp("", "scan-hub-demo-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(d)
		dir = d
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ss, err := venstest.Start(venstest.Options{Password: "demo", EndOfBatchQuirk: true, HeartbeatTimeout: 3 * time.Second})
	if err != nil {
		return err
	}
	defer ss.Close()
	ss.LoadPaper(paper)
	mgr := scansnap.New(scansnap.Config{
		ScannerIP: ss.IP(), Password: "demo", JournalPath: filepath.Join(dir, "scansnap", "journal.json"),
		ControlPort: ss.ControlPort(), DataPort: ss.DataPort(), DiscoveryPort: ss.DiscoveryPort(),
		DiscoveryReplyPort: freeUDP(), NotifyPort: freeUDP(), StatusInterval: 2 * time.Second, Logger: log,
	})
	done := make(chan struct{})
	go func() { mgr.Run(ctx); close(done) }()
	defer func() { stop(); <-done }()
	// Keep the fake feeder stocked so every scan has paper.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if mgr.Status().State == scansnap.StateClaimed {
					ss.LoadPaper(paper)
				}
			}
		}
	}()

	ep := escltest.Start()
	defer ep.Close()
	epc, err := escl.New(escl.Options{BaseURL: ep.URL, TLSPin: escl.SPKIPin(ep.Certificate()), Logger: log})
	if err != nil {
		return err
	}

	pl := paperlesstest.Start("demo")
	defer pl.Close()
	pl.Set(func(s *paperlesstest.Server) { s.PendingFor = 3 })
	pc, _ := paperless.NewClient(pl.URL, "demo", nil)
	ob, err := paperless.Open(pc, paperless.Options{Dir: filepath.Join(dir, "outbox"), MinFreeBytes: -1, Logger: log})
	if err != nil {
		return err
	}
	defer ob.Close()
	obDone := make(chan struct{})
	go func() { ob.Run(ctx); close(obDone) }()
	defer func() { stop(); <-obDone }()

	store, err := session.Open(filepath.Join(dir, "sessions"), session.Options{MinFreeBytes: -1})
	if err != nil {
		return err
	}
	btn := vens.DefaultScanConfig()
	btn.Quality = vens.QualitySuperFine
	ui, err := web.New(ctx, web.Config{
		AllowedHosts: []string{addr, "localhost:" + port},
		KeyPath:      filepath.Join(dir, "web", "csrf.key"),
		ScanSnap:     btn,
		Epson:        config.EpsonProfile{ColorMode: escl.ColorRGB24, DPI: 300, Region: "a4"},
		PaperlessURL: "https://paperless.example",
		Logger:       log,
	}, web.Deps{ScanSnap: mgr, Epson: epc, Store: store, Outbox: ob, Paperless: pc, DataDir: dir})
	if err != nil {
		return err
	}
	defer ui.Wait()
	srv := &http.Server{Addr: addr, Handler: ui.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	log.Info("demo UI with fake devices", "url", "http://"+addr, "data", dir)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
