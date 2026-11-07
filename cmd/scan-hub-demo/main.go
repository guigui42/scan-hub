// Command scan-hub-demo runs the real web UI against fake devices (two
// ScanSnaps, two eSCL scanners and a Paperless on loopback) for UI work and
// browser tests. It never talks to real hardware.
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
	"sync"
	"syscall"
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

	// Two fake ScanSnaps share one button listener and the discovery reply
	// port, like real ones. The second needs 127.0.0.2 (Linux, or a lo0
	// alias on macOS).
	hub := scansnap.NewNotifyHub(0, log)
	if err := hub.Listen(); err != nil {
		return err
	}
	defer hub.Close()
	reply := freeUDP()
	var (
		devs  []scanner.Device
		fakes []*venstest.Scanner
		mgrs  []*scansnap.Manager
		snaps sync.WaitGroup
	)
	for i, sn := range []struct{ id, name, ip string }{
		{"feeder", "ScanSnap iX500", "127.0.0.1"},
		{"feeder-2", "Second ScanSnap", "127.0.0.2"},
	} {
		ss, err := venstest.Start(venstest.Options{IP: sn.ip, Password: "demo", EndOfBatchQuirk: true, HeartbeatTimeout: 3 * time.Second, HeartbeatACK: true})
		if err != nil {
			if i == 0 {
				return err
			}
			log.Warn("second fake ScanSnap disabled", "ip", sn.ip, "err", err, "hint", "macOS: sudo ifconfig lo0 alias "+sn.ip)
			continue
		}
		defer ss.Close()
		ss.LoadPaper(paper)
		mgr := scansnap.New(scansnap.Config{
			ScannerIP: ss.IP(), Password: "demo", JournalPath: scansnap.JournalPath(filepath.Join(dir, "scansnap"), sn.id),
			ControlPort: ss.ControlPort(), DataPort: ss.DataPort(), DiscoveryPort: ss.DiscoveryPort(),
			DiscoveryReplyPort: reply, Hub: hub, StatusInterval: 2 * time.Second, Logger: log.With("scanner", sn.id),
		})
		dev := scansnapdrv.NewDevice(sn.id, sn.name, mgr, scansnapdrv.DefaultConfig(), scansnapdrv.DefaultConfig())
		snaps.Add(1)
		go func() { defer snaps.Done(); dev.Run(ctx) }()
		devs, fakes, mgrs = append(devs, dev), append(fakes, ss), append(mgrs, mgr)
	}
	// Runs first: the managers release the fakes before they and the hub close.
	defer func() { stop(); snaps.Wait() }()
	// Keep the fake feeders stocked so every scan has paper.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for i, m := range mgrs {
					if m.Status().State == scansnap.StateClaimed {
						fakes[i].LoadPaper(paper)
					}
				}
			}
		}
	}()

	for _, ep := range []struct{ id, name string }{{"glass", "Epson ET-2750"}, {"glass-2", "Office AirScan"}} {
		f := escltest.Start()
		defer f.Close()
		c, err := escl.New(escl.Options{BaseURL: f.URL, TLSPin: escl.SPKIPin(f.Certificate()), Logger: log.With("scanner", ep.id)})
		if err != nil {
			return err
		}
		devs = append(devs, escldrv.NewDevice(ep.id, ep.name, c, escldrv.DefaultProfile, 0))
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
	ui, err := web.New(ctx, web.Config{
		AllowedHosts: []string{addr, "localhost:" + port},
		KeyPath:      filepath.Join(dir, "web", "csrf.key"),
		PaperlessURL: "https://paperless.example",
		Logger:       log,
	}, web.Deps{Scanners: devs, Store: store, Outbox: ob, Paperless: pc, DataDir: dir})
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
