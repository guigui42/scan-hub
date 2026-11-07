// Command scan-hub bridges Wi-Fi scanners to Paperless-ngx.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/guigui42/scan-hub/internal/config"
	"github.com/guigui42/scan-hub/internal/escl"
	"github.com/guigui42/scan-hub/internal/paperless"
	"github.com/guigui42/scan-hub/internal/pipeline"
	"github.com/guigui42/scan-hub/internal/scansnap"
	"github.com/guigui42/scan-hub/internal/session"
	"github.com/guigui42/scan-hub/internal/vens"
	"github.com/guigui42/scan-hub/internal/web"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		cmds := map[string]func([]string) error{"send": sendCmd, "pdf": pdfCmd, "scan-epson": scanEpsonCmd}
		switch name := os.Args[1]; {
		case name == "version" || name == "--version":
			fmt.Println(version)
			return
		case name == "healthcheck":
			os.Exit(healthcheck())
		case cmds[name] != nil:
			if err := cmds[name](os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
				os.Exit(1)
			}
			return
		default:
			fmt.Fprintln(os.Stderr, "usage: scan-hub [version | healthcheck | send <session> | pdf <session> | scan-epson]")
			os.Exit(2)
		}
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// workers tracks background goroutines for /readyz.
type workers struct {
	mu    sync.Mutex
	alive map[string]*atomic.Bool
	wg    sync.WaitGroup
}

func (w *workers) start(name string, fn func()) {
	w.mu.Lock()
	if w.alive == nil {
		w.alive = map[string]*atomic.Bool{}
	}
	b := &atomic.Bool{}
	b.Store(true)
	w.alive[name] = b
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer b.Store(false)
		fn()
	}()
}

func (w *workers) ready() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var dead []string
	for name, b := range w.alive {
		if !b.Load() {
			dead = append(dead, name)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		return fmt.Errorf("stopped workers: %s", strings.Join(dead, ", "))
	}
	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	log.Info("scan-hub starting", "version", version, "config", cfg)

	store, err := openStore(cfg)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	var (
		w  workers
		pc *paperless.Client
		ob *paperless.Outbox
		ui *web.Server
	)
	// Shutdown order: stop work, let scan jobs and workers finish (the
	// ScanSnap is released by its actor), then drop the outbox lock.
	defer func() {
		stop()
		if ui != nil {
			ui.Wait()
		}
		w.wg.Wait()
		if ob != nil {
			ob.Close()
		}
	}()

	if cfg.PaperlessURL != "" {
		if pc, ob, err = openOutbox(cfg, log); err != nil {
			return fmt.Errorf("outbox: %w", err)
		}
		w.start("outbox", func() { ob.Run(ctx) })
	} else {
		log.Warn("SCANHUB_PAPERLESS_URL not set: uploads disabled")
	}

	var ep *escl.Client
	if cfg.EpsonURL != "" {
		if ep, err = newEpson(cfg, log); err != nil {
			return err
		}
	} else {
		log.Warn("SCANHUB_EPSON_URL not set: Epson disabled")
	}

	var ss *scansnap.Manager
	if cfg.ScanSnapIP != "" {
		ss = scansnap.New(scansnap.Config{
			ScannerIP:   cfg.ScanSnapIP,
			Password:    cfg.ScanSnapPassword,
			JournalPath: filepath.Join(cfg.DataDir, "scansnap", "journal.json"),
			Logger:      log,
		})
		runErr := make(chan error, 1)
		w.start("scansnap", func() { runErr <- ss.Run(ctx) })
		// Surface an immediate startup failure (e.g. UDP 55265 in use).
		select {
		case err := <-runErr:
			if err != nil {
				return err
			}
		case <-time.After(200 * time.Millisecond):
		}
		w.start("button", func() { buttonLoop(ctx, log, ss, store, ob, cfg) })
	} else {
		log.Warn("SCANHUB_SCANSNAP_IP not set: ScanSnap disabled")
	}
	w.start("cleanup", func() { cleanupLoop(ctx, log, store, cfg.SessionMaxAge) })

	ui, err = web.New(ctx, web.Config{
		AllowedHosts: cfg.AllowedHosts,
		KeyPath:      filepath.Join(cfg.DataDir, "web", "csrf.key"),
		ScanSnap:     cfg.Button,
		Epson:        cfg.Epson,
		PaperlessURL: cfg.PaperlessURL,
		Logger:       log,
	}, web.Deps{ScanSnap: ss, Epson: ep, Store: store, Outbox: ob, Paperless: pc, Ready: w.ready, DataDir: cfg.DataDir})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           ui.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	srvErr := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.ListenAddr, "allowed_hosts", cfg.AllowedHosts)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		return fmt.Errorf("http: %w", err)
	}
	log.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shCtx)
	return nil
}

// buttonLoop turns each physical Scan press into a batch with the button
// profile. A clean batch goes straight to the Paperless outbox; a batch that
// stopped early (jam, multi-feed) stays on the desk for review.
func buttonLoop(ctx context.Context, log *slog.Logger, ss *scansnap.Manager, store *session.Store, ob *paperless.Outbox, cfg config.Config) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ss.Buttons():
			if st := ss.Status().State; st != scansnap.StateClaimed {
				log.Warn("button press ignored", "state", st, "seq", ev.Seq)
				continue
			}
			// The session is created with the first page, so a press with an
			// empty feeder leaves nothing behind.
			var id string
			res := ss.Scan(ctx, scansnap.ScanRequest{Config: cfg.Button, OnPage: func(p scansnap.Page) error {
				if id == "" {
					m, err := store.Create("scansnap-button")
					if err != nil {
						return err
					}
					id = m.ID
				}
				_, err := store.AddPage(id, p.Data, "scansnap", p.Sheet, p.Side)
				return err
			}})
			var se *vens.ScanError
			switch {
			case errors.Is(res.Err, scansnap.ErrBusy):
				log.Warn("button press ignored: scan already running", "seq", ev.Seq)
			case errors.As(res.Err, &se) && (se.Kind == vens.ScanErrNoPaper || se.Kind == vens.ScanErrCoverOpen):
				log.Warn("button press: "+se.Msg, "seq", ev.Seq)
			case res.Err != nil:
				log.Error("button scan stopped early; pages kept on the desk", "session", id, "pages", res.Pages, "err", res.Err)
			case id == "":
				log.Warn("button scan produced no pages", "seq", ev.Seq)
			case ob == nil:
				log.Info("button scan stored (Paperless not configured)", "session", id, "pages", res.Pages)
			default:
				if _, err := pipeline.Submit(log, store, ob, id); err != nil {
					log.Error("button scan: hand-off to outbox failed; pages kept on the desk", "session", id, "err", err)
				}
			}
		}
	}
}

// healthcheck is the container HEALTHCHECK (the image has no shell or curl).
func healthcheck() int {
	addr := os.Getenv("SCANHUB_LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8097"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
