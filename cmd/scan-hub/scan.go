package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/guigui42/scan-hub/internal/config"
	"github.com/guigui42/scan-hub/internal/scanner"
)

// setFlags collects repeated -set key=value.
type setFlags map[string]string

func (s setFlags) String() string { return fmt.Sprint(map[string]string(s)) }
func (s setFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", v)
	}
	s[k] = val
	return nil
}

// scanCmd runs one scan into a new session and prints its id:
//
//	scan-hub scan <scanner-id> [-set color=gray -set dpi=600 ...]
//
// Settings default to the scanner list's; keys are the UI form fields.
// Scanners held by the server (ScanSnaps) can't be used from here.
func scanCmd(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: scan-hub scan <scanner-id> [-set key=value ...]")
	}
	id := args[0]
	set := setFlags{}
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.Var(set, "set", "scan setting key=value (repeatable); keys as in the UI form, checkboxes take on/off")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	devs, ss, err := loadScanners(cfg, log)
	if err != nil {
		return fmt.Errorf("scanners: %w", err)
	}
	defer ss.Close()
	dev, ok := scanner.Find(devs, id)
	if !ok {
		return fmt.Errorf("no scanner %q in the list (have: %s)", id, strings.Join(scannerIDs(devs), ", "))
	}
	if _, ok := dev.(scanner.Runner); ok {
		return fmt.Errorf("%s is held by the server: scan from the web UI or its Scan button", dev.Name())
	}

	values := map[string]string{}
	known := map[string]bool{}
	for _, f := range dev.Form() {
		known[f.Name] = true
		values[f.Name] = f.Value
		if f.Type == scanner.Checkbox && f.Checked {
			values[f.Name] = "on"
		}
	}
	for k, v := range set {
		if !known[k] {
			return fmt.Errorf("unknown setting %q for %s", k, id)
		}
		if v == "off" {
			v = ""
		}
		values[k] = v
	}
	settings, err := dev.ParseForm(func(k string) string { return values[k] })
	if err != nil {
		return err
	}

	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var sid string
	err = dev.Scan(ctx, settings, func(p scanner.Page) error {
		if sid == "" {
			m, err := store.Create("cli:" + id)
			if err != nil {
				return err
			}
			sid = m.ID
		}
		pg, err := store.AddPage(sid, p.Data, id, p.Sheet, p.Side)
		if err == nil {
			log.Info("page stored", "session", sid, "file", pg.File, "width", pg.Width, "height", pg.Height, "dpi", pg.DPI, "bytes", pg.Size)
		}
		return err
	})
	if sid != "" {
		fmt.Println(sid)
	}
	if err != nil {
		return err
	}
	if sid == "" {
		return errors.New("the scan returned no pages")
	}
	return nil
}
