package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/guigui42/scan-hub/internal/config"
	"github.com/guigui42/scan-hub/internal/fsutil"
	"github.com/guigui42/scan-hub/internal/pdf"
	"github.com/guigui42/scan-hub/internal/pipeline"
	"github.com/guigui42/scan-hub/internal/session"
)

func openStore(cfg config.Config) (*session.Store, error) {
	maxPages, minFree := cfg.SessionOptions()
	opt := session.Options{MaxPages: maxPages, MinFreeBytes: minFree}
	if minFree == 0 {
		opt.MinFreeBytes = -1 // SCANHUB_MIN_FREE_MB=0 disables the check
	}
	return session.Open(filepath.Join(cfg.DataDir, "sessions"), opt)
}

// pdfCmd writes a session as one PDF:
//
//	scan-hub pdf <session-id> [-o file.pdf]
func pdfCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	fs := flag.NewFlagSet("pdf", flag.ContinueOnError)
	out := fs.String("o", "", "output file (default <session-id>.pdf)")
	if len(args) == 0 {
		return errors.New("usage: scan-hub pdf <session-id> [-o file.pdf]")
	}
	id := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *out == "" {
		*out = id + ".pdf"
	}
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	m, err := store.Get(id)
	if err != nil {
		return fmt.Errorf("session %s: %w", id, err)
	}
	f, err := os.CreateTemp(filepath.Dir(*out), ".scan-hub-*.pdf")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := pdf.Write(f, pipeline.PDFPages(store, m), pdf.Options{Title: m.Title, Created: m.Created}); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), *out); err != nil {
		return err
	}
	fsutil.SyncDir(filepath.Dir(*out))
	fmt.Println(*out)
	return nil
}

// cleanupLoop deletes abandoned sessions hourly (and once at start).
func cleanupLoop(ctx context.Context, log *slog.Logger, store *session.Store, maxAge time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		gone, err := store.CleanupAbandoned(maxAge, time.Now())
		if err != nil {
			log.Warn("session cleanup", "err", err)
		}
		for _, id := range gone {
			log.Info("deleted abandoned session", "session", id, "max_age", maxAge)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
