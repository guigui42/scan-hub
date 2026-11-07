// Package escldrv drives eSCL (AirScan) scanners' flatbed through
// internal/escl.
package escldrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/guigui42/scan-hub/internal/escl"
	"github.com/guigui42/scan-hub/internal/scanner"
	"github.com/guigui42/scan-hub/internal/scannerconfig"
)

// Type is the scanner-list type of this driver.
const Type = "escl"

// Profile is a flatbed scan in user terms.
type Profile struct {
	Color  string // color, gray, bw
	DPI    string // 100, 200, 300, 600
	Region string // a4, letter, max
}

// DefaultProfile is used for keys missing from the scanner list.
var DefaultProfile = Profile{Color: "color", DPI: "300", Region: "a4"}

// Settings converts a profile to eSCL settings.
func (p Profile) Settings() (escl.ScanSettings, error) {
	var (
		s    escl.ScanSettings
		errs []error
	)
	switch strings.ToLower(p.Color) {
	case "color", "colour":
		s.ColorMode = escl.ColorRGB24
	case "gray", "grey":
		s.ColorMode = escl.ColorGrayscale
	case "bw":
		s.ColorMode = escl.ColorBW
	default:
		errs = append(errs, fmt.Errorf("color: want color|gray|bw, got %q", p.Color))
	}
	switch p.DPI {
	case "100", "200", "300", "600":
		s.DPI, _ = strconv.Atoi(p.DPI)
	default:
		errs = append(errs, fmt.Errorf("dpi: want 100|200|300|600, got %q", p.DPI))
	}
	switch strings.ToLower(p.Region) {
	case "a4":
		s.Region = escl.RegionA4
	case "letter":
		s.Region = escl.RegionLetter
	case "max": // zero region: the whole platen
	default:
		errs = append(errs, fmt.Errorf("region: want a4|letter|max, got %q", p.Region))
	}
	return s, errors.Join(errs...)
}

// Options of the driver.
type Options struct {
	Logger    *slog.Logger
	StatusTTL time.Duration // default 10 s
}

// Driver builds eSCL devices from the scanner list.
type Driver struct{ opts Options }

// New returns the driver.
func New(o Options) *Driver {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Driver{opts: o}
}

type entry struct {
	scannerconfig.Common `yaml:",inline"`
	URL                  string `yaml:"url"`
	TLSPin               string `yaml:"tls_pin"`
	TLSInsecure          bool   `yaml:"tls_insecure"`
	Defaults             struct {
		Color  string `yaml:"color"`
		DPI    string `yaml:"dpi"`
		Region string `yaml:"region"`
	} `yaml:"defaults"`
}

// Build implements scannerconfig.Driver.
func (d *Driver) Build(c scannerconfig.Common, decode func(any) error) (scanner.Device, error) {
	var e entry
	if err := decode(&e); err != nil {
		return nil, err
	}
	var errs []error
	https := strings.HasPrefix(e.URL, "https://")
	switch {
	case e.URL == "":
		errs = append(errs, errors.New("url is required"))
	case !https && !strings.HasPrefix(e.URL, "http://"):
		errs = append(errs, fmt.Errorf("url: want http:// or https://, got %q", e.URL))
	case https && (e.TLSPin == "") == !e.TLSInsecure:
		errs = append(errs, errors.New("an https url needs exactly one of tls_pin and tls_insecure: true"))
	case !https && (e.TLSPin != "" || e.TLSInsecure):
		errs = append(errs, errors.New("tls_pin and tls_insecure apply to https urls only"))
	}
	p := DefaultProfile
	for _, kv := range []struct {
		dst *string
		v   string
	}{{&p.Color, e.Defaults.Color}, {&p.DPI, e.Defaults.DPI}, {&p.Region, e.Defaults.Region}} {
		if kv.v != "" {
			*kv.dst = kv.v
		}
	}
	if _, err := p.Settings(); err != nil {
		errs = append(errs, fmt.Errorf("defaults: %w", err))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	cl, err := escl.New(escl.Options{BaseURL: e.URL, TLSPin: e.TLSPin, Insecure: e.TLSInsecure, Logger: d.opts.Logger.With("scanner", c.ID)})
	if err != nil {
		return nil, err
	}
	return NewDevice(c.ID, c.Name, cl, p, d.opts.StatusTTL), nil
}

// Device is one eSCL scanner.
type Device struct {
	id, name string
	c        *escl.Client
	defaults Profile
	ttl      time.Duration

	mu     sync.Mutex
	at     time.Time
	status scanner.Status
}

// NewDevice wraps a client; ttl 0 caches status for 10 s.
func NewDevice(id, name string, c *escl.Client, defaults Profile, ttl time.Duration) *Device {
	if ttl == 0 {
		ttl = 10 * time.Second
	}
	return &Device{id: id, name: name, c: c, defaults: defaults, ttl: ttl}
}

func (d *Device) ID() string         { return d.id }
func (d *Device) Name() string       { return d.name }
func (d *Device) Kind() scanner.Kind { return scanner.Flatbed }
func (d *Device) Ready() error       { return nil }

// Status probes the scanner at most once per TTL. The probe doesn't inherit
// the caller's cancellation, so an aborted request isn't cached as
// "unreachable".
func (d *Device) Status(ctx context.Context) scanner.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.at.IsZero() && time.Since(d.at) < d.ttl {
		return d.status
	}
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	st, err := d.c.Status(c)
	state, detail := st.State, ""
	if err != nil {
		state, detail = "Unreachable", err.Error()
	}
	d.status = scanner.Status{
		Tone:   tone(state),
		Label:  label(state),
		Detail: detail,
		Facts:  []scanner.Fact{{Label: "Input", Value: "Flatbed, one page per pass"}},
		Raw:    map[string]string{"state": state, "detail": detail},
	}
	d.at = time.Now()
	return d.status
}

func tone(state string) scanner.Tone {
	switch state {
	case "Idle":
		return scanner.ToneOK
	case "Processing", "Testing":
		return scanner.ToneBusy
	case "Unreachable", "Down", "Stopped":
		return scanner.ToneBad
	}
	return scanner.ToneIdle
}

func label(state string) string {
	if state == "Idle" {
		return "Ready"
	}
	return state
}

func (d *Device) Form() []scanner.Field {
	return []scanner.Field{
		{Name: "color", Label: "Colour", Type: scanner.Select, Value: d.defaults.Color, Options: []scanner.Option{
			{Value: "color", Label: "Colour"}, {Value: "gray", Label: "Grey"}, {Value: "bw", Label: "Black & white"}}},
		{Name: "dpi", Label: "Resolution", Type: scanner.Select, Value: d.defaults.DPI, Options: []scanner.Option{
			{Value: "100", Label: "100 dpi"}, {Value: "200", Label: "200 dpi"}, {Value: "300", Label: "300 dpi"}, {Value: "600", Label: "600 dpi"}}},
		{Name: "region", Label: "Area", Type: scanner.Select, Value: d.defaults.Region, Options: []scanner.Option{
			{Value: "a4", Label: "A4"}, {Value: "letter", Label: "Letter"}, {Value: "max", Label: "Whole glass"}}},
	}
}

func (d *Device) ParseForm(get func(string) string) (scanner.Settings, error) {
	return Profile{Color: get("color"), DPI: get("dpi"), Region: get("region")}.Settings()
}

// DefaultSettings are the scanner-list defaults (for the CLI).
func (d *Device) DefaultSettings() Profile { return d.defaults }

func (d *Device) Scan(ctx context.Context, s scanner.Settings, onPage func(scanner.Page) error) error {
	st, ok := s.(escl.ScanSettings)
	if !ok {
		return fmt.Errorf("escldrv: unexpected settings %T", s)
	}
	docs, err := d.c.Scan(ctx, st)
	if errors.Is(err, escl.ErrBusy) {
		return fmt.Errorf("%w: %w", scanner.ErrBusy, err)
	}
	if err != nil {
		return err
	}
	for _, doc := range docs {
		if err := onPage(scanner.Page{Data: doc.Data}); err != nil {
			return err
		}
	}
	return nil
}
