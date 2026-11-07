package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/guigui42/scan-hub/internal/config"
	"github.com/guigui42/scan-hub/internal/paperless"
	"github.com/guigui42/scan-hub/internal/pipeline"
)

func openOutbox(cfg config.Config, log *slog.Logger) (*paperless.Client, *paperless.Outbox, error) {
	pc, err := paperless.NewClient(cfg.PaperlessURL, cfg.PaperlessToken, &http.Client{Timeout: 0})
	if err != nil {
		return nil, nil, err
	}
	opt := paperless.Options{
		Dir:      filepath.Join(cfg.DataDir, "outbox"),
		Tags:     cfg.PaperlessTags,
		MaxBytes: cfg.OutboxMaxMB << 20,
		Logger:   log,
	}
	if cfg.MinFreeMB == 0 {
		opt.MinFreeBytes = -1
	} else {
		opt.MinFreeBytes = cfg.MinFreeMB << 20
	}
	ob, err := paperless.Open(pc, opt)
	if err != nil {
		return nil, nil, err
	}
	return pc, ob, nil
}

// sendCmd submits a session and waits for Paperless:
//
//	scan-hub send <session-id> [-wait 5m]
func sendCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if cfg.PaperlessURL == "" {
		return errors.New("SCANHUB_PAPERLESS_URL is not set")
	}
	if len(args) == 0 {
		return errors.New("usage: scan-hub send <session-id> [-wait 5m]")
	}
	id := args[0]
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	waitFor := fs.Duration("wait", 5*time.Minute, "how long to wait for Paperless")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	_, ob, err := openOutbox(cfg, log)
	if errors.Is(err, paperless.ErrLocked) {
		return fmt.Errorf("%w; send it from the running server instead", err)
	}
	if err != nil {
		return err
	}
	defer ob.Close()
	it, err := pipeline.Submit(log, store, ob, id)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *waitFor)
	done := make(chan struct{})
	go func() { ob.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		cur, _ := ob.Get(it.ID)
		if cur.State.Done() || cur.State == paperless.StateFailure {
			fmt.Printf("%s %s document=%d duplicate_of=%d %s\n", cur.ID, cur.State, cur.DocumentID, cur.DuplicateOf, cur.Detail)
			if cur.State == paperless.StateFailure {
				return errors.New("Paperless did not accept the document (kept in the outbox)")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Printf("%s %s %s (still in the outbox)\n", cur.ID, cur.State, cur.Detail)
			return ctx.Err()
		case <-t.C:
		}
	}
}
