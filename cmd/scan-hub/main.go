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
	"github.com/guigui42/scan-hub/internal/paperless"
	"github.com/guigui42/scan-hub/internal/pipeline"
	"github.com/guigui42/scan-hub/internal/scanner"
	"github.com/guigui42/scan-hub/internal/session"
	"github.com/guigui42/scan-hub/internal/web"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		cmds := map[string]func([]string) error{"send": sendCmd, "pdf": pdfCmd, "scan": scanCmd}
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
			fmt.Fprintln(os.Stderr, "usage: scan-hub [version | healthcheck | send <session> | pdf <session> | scan <scanner>]")
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
	alive map[string]*worker
	wg    sync.WaitGroup
}

type worker struct {
	alive atomic.Bool
	err   atomic.Pointer[error]
}

// start runs fn; its error is reported by ready once it has stopped.
func (w *workers) start(name string, fn func() error) {
	w.mu.Lock()
	if w.alive == nil {
		w.alive = map[string]*worker{}
	}
	st := &worker{}
	st.alive.Store(true)
	w.alive[name] = st
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer st.alive.Store(false)
		if err := fn(); err != nil {
			st.err.Store(&err)
		}
	}()
}

func (w *workers) ready() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var dead []string
	for name, st := range w.alive {
		if !st.alive.Load() {
			if err := st.err.Load(); err != nil {
				name += " (" + (*err).Error() + ")"
			}
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
	devs, ssd, err := loadScanners(cfg, log)
	if err != nil {
		stop()
		return fmt.Errorf("scanners: %w", err)
	}
	var (
		w  workers
		pc *paperless.Client
		ob *paperless.Outbox
		ui *web.Server
	)
	// Shutdown order: stop work, let scan jobs and workers finish (each
	// ScanSnap is released by its actor), close the shared button listener,
	// then drop the outbox lock.
	defer func() {
		stop()
		if ui != nil {
			ui.Wait()
		}
		w.wg.Wait()
		ssd.Close()
		if ob != nil {
			ob.Close()
		}
	}()

	if cfg.PaperlessURL != "" {
		if pc, ob, err = openOutbox(cfg, log); err != nil {
			return fmt.Errorf("outbox: %w", err)
		}
		w.start("outbox", func() error { ob.Run(ctx); return nil })
	} else {
		log.Warn("SCANHUB_PAPERLESS_URL not set: uploads disabled")
	}

	if len(devs) == 0 {
		log.Warn("no scanners configured (SCANHUB_SCANNERS_FILE): only the outbox works")
	}
	if err := ssd.PrepareJournals(); err != nil {
		return fmt.Errorf("ScanSnap journals: %w", err)
	}
	// Bound before any ScanSnap runs, so a port conflict stops startup here.
	if err := ssd.Listen(); err != nil {
		return err
	}
	runErrs := make(chan error, len(devs))
	for _, d := range devs {
		log.Info("scanner", "id", d.ID(), "name", d.Name(), "kind", d.Kind())
		if r, ok := d.(scanner.Runner); ok {
			w.start("scanner:"+d.ID(), func() error {
				err := r.Run(ctx)
				if err != nil {
					runErrs <- fmt.Errorf("scanner %s: %w", d.ID(), err)
				}
				return err
			})
		}
		if b, ok := d.(scanner.Button); ok {
			w.start("button:"+d.ID(), func() error { buttonLoop(ctx, log, d, b, store, ob); return nil })
		}
	}
	// Surface an immediate startup failure of a scanner.
	select {
	case err := <-runErrs:
		return err
	case <-time.After(200 * time.Millisecond):
	}
	w.start("cleanup", func() error { cleanupLoop(ctx, log, store, cfg.SessionMaxAge); return nil })

	ui, err = web.New(ctx, web.Config{
		AllowedHosts: cfg.AllowedHosts,
		KeyPath:      filepath.Join(cfg.DataDir, "web", "csrf.key"),
		PaperlessURL: cfg.PaperlessURL,
		Logger:       log,
	}, web.Deps{Scanners: devs, Store: store, Outbox: ob, Paperless: pc, Ready: w.ready, DataDir: cfg.DataDir})
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

// buttonLoop turns each press of a scanner's own Scan button into a batch
// with its button profile. A clean batch goes straight to the Paperless
// outbox; a batch that stopped early (jam, multi-feed) stays on the desk for
// review. It returns when the device stops.
func buttonLoop(ctx context.Context, log *slog.Logger, dev scanner.Device, b scanner.Button, store *session.Store, ob *paperless.Outbox) {
	log = log.With("scanner", dev.ID())
	for {
		var ev scanner.ButtonEvent
		select {
		case <-ctx.Done():
			return
		case e, ok := <-b.Buttons():
			if !ok {
				return
			}
			ev = e
		}
		if err := dev.Ready(); err != nil {
			log.Warn("button press ignored", "reason", err.Error(), "seq", ev.Seq)
			continue
		}
		// The session is created with the first page, so a press with an
		// empty feeder leaves nothing behind.
		var id string
		pages := 0
		err := dev.Scan(ctx, b.ButtonSettings(), func(p scanner.Page) error {
			if id == "" {
				m, err := store.Create("button:" + dev.ID())
				if err != nil {
					return err
				}
				id = m.ID
			}
			if _, err := store.AddPage(id, p.Data, dev.ID(), p.Sheet, p.Side); err != nil {
				return err
			}
			pages++
			return nil
		})
		switch {
		case errors.Is(err, scanner.ErrBusy):
			log.Warn("button press ignored: scan already running", "seq", ev.Seq)
		case errors.Is(err, scanner.ErrNoPaper), errors.Is(err, scanner.ErrCoverOpen):
			log.Warn("button press: "+err.Error(), "seq", ev.Seq)
		case err != nil:
			log.Error("button scan stopped early; pages kept on the desk", "session", id, "pages", pages, "err", err)
		case id == "":
			log.Warn("button scan produced no pages", "seq", ev.Seq)
		case ob == nil:
			log.Info("button scan stored (Paperless not configured)", "session", id, "pages", pages)
		default:
			if _, err := pipeline.Submit(log, store, ob, id); err != nil {
				log.Error("button scan: hand-off to outbox failed; pages kept on the desk", "session", id, "err", err)
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
