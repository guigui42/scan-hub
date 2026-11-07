package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/guigui42/scan-hub/internal/config"
	"github.com/guigui42/scan-hub/internal/escl"
)

func newEpson(cfg config.Config, log *slog.Logger) (*escl.Client, error) {
	return escl.New(escl.Options{BaseURL: cfg.EpsonURL, TLSPin: cfg.EpsonTLSPin, Insecure: cfg.EpsonInsecure, Logger: log})
}

func epsonSettings(p config.EpsonProfile) escl.ScanSettings {
	s := escl.ScanSettings{ColorMode: p.ColorMode, DPI: p.DPI}
	switch p.Region {
	case "a4":
		s.Region = escl.RegionA4
	case "letter":
		s.Region = escl.RegionLetter
	}
	return s
}

// scanEpsonCmd runs one flatbed pass into a new session:
//
//	scan-hub scan-epson [-color color|gray|bw] [-dpi 300] [-region a4|letter|max]
func scanEpsonCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if cfg.EpsonURL == "" {
		return errors.New("SCANHUB_EPSON_URL is not set")
	}
	fs := flag.NewFlagSet("scan-epson", flag.ContinueOnError)
	color := fs.String("color", "", "color, gray or bw (default from SCANHUB_EPSON_COLOR)")
	dpi := fs.String("dpi", "", "100, 200, 300 or 600 (default from SCANHUB_EPSON_DPI)")
	region := fs.String("region", "", "a4, letter or max (default from SCANHUB_EPSON_REGION)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prof := cfg.Epson
	if *color != "" || *dpi != "" || *region != "" {
		c, d, r := profileStrings(prof)
		if *color != "" {
			c = *color
		}
		if *dpi != "" {
			d = *dpi
		}
		if *region != "" {
			r = *region
		}
		if prof, err = config.ParseEpsonProfile(c, d, r); err != nil {
			return err
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ep, err := newEpson(cfg, log)
	if err != nil {
		return err
	}
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	docs, err := ep.Scan(ctx, epsonSettings(prof))
	if err != nil {
		return err
	}
	m, err := store.Create("epson")
	if err != nil {
		return err
	}
	for _, d := range docs {
		p, err := store.AddPage(m.ID, d.Data, "epson", 0, 0)
		if err != nil {
			return err
		}
		log.Info("page stored", "session", m.ID, "file", p.File, "width", p.Width, "height", p.Height, "dpi", p.DPI, "bytes", p.Size)
	}
	fmt.Println(m.ID)
	return nil
}

func profileStrings(p config.EpsonProfile) (color, dpi, region string) {
	switch p.ColorMode {
	case escl.ColorGrayscale:
		color = "gray"
	case escl.ColorBW:
		color = "bw"
	default:
		color = "color"
	}
	return color, fmt.Sprint(p.DPI), p.Region
}
