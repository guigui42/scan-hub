// Package scansnapdrv drives ScanSnap iX500 scanners (VENS) through
// internal/scansnap. Several ScanSnaps share one button-event listener and
// take turns for discovery.
package scansnapdrv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/guigui42/scan-hub/internal/scanner"
	"github.com/guigui42/scan-hub/internal/scannerconfig"
	"github.com/guigui42/scan-hub/internal/scansnap"
	"github.com/guigui42/scan-hub/internal/vens"
)

// Type is the scanner-list type of this driver.
const Type = "scansnap"

// Options of the driver.
type Options struct {
	DataDir    string // the ownership journals go under DataDir/scansnap
	Logger     *slog.Logger
	LookupEnv  func(string) (string, bool) // for password_env
	NotifyPort int                         // local button-event port; 0 = 55265
	// AnnouncePort receives power-on announcements; 0 = 53220, -1 = don't
	// listen (restarted scanners are then reclaimed on the retry schedule).
	AnnouncePort int
	// Tune adjusts each manager's configuration (tests and the demo).
	Tune func(id string, c *scansnap.Config)
}

// Driver builds ScanSnap devices from the scanner list and owns their shared
// button-event listener. Close it after every device has stopped.
type Driver struct {
	opts Options
	hub  *scansnap.NotifyHub
	ips  map[netip.Addr]string // scanner IP -> id
	ids  map[string]string     // id -> scanner IP
}

// New returns the driver; nothing is bound until Listen.
func New(o Options) *Driver {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.NotifyPort == 0 {
		o.NotifyPort = vens.ClientNotifyPort
	}
	if o.AnnouncePort == 0 {
		o.AnnouncePort = vens.BroadcastPort
	}
	return &Driver{opts: o, ips: map[netip.Addr]string{}, ids: map[string]string{}}
}

// JournalDir is where the ownership journals live.
func (d *Driver) JournalDir() string { return filepath.Join(d.opts.DataDir, "scansnap") }

// PrepareJournals checks and migrates the ownership journals of the built
// ScanSnaps (see scansnap.PrepareJournals). Call it before running them.
func (d *Driver) PrepareJournals() error {
	return scansnap.PrepareJournals(d.JournalDir(), d.ids)
}

// Listen binds the shared button-event port, before any device runs. One-shot
// commands that only read the list don't call it.
func (d *Driver) Listen() error {
	if d.hub == nil {
		return nil
	}
	return d.hub.Listen()
}

// Close releases the shared listener.
func (d *Driver) Close() error {
	if d.hub == nil {
		return nil
	}
	return d.hub.Close()
}

type profile struct {
	Color            string `yaml:"color"`
	DPI              string `yaml:"dpi"`
	Paper            string `yaml:"paper"`
	Duplex           *bool  `yaml:"duplex"`
	BlankPageRemoval *bool  `yaml:"blank_page_removal"`
	MultiFeed        *bool  `yaml:"multifeed"`
}

type entry struct {
	scannerconfig.Common `yaml:",inline"`
	IP                   string   `yaml:"ip"`
	PasswordFile         string   `yaml:"password_file"`
	PasswordEnv          string   `yaml:"password_env"`
	Defaults             profile  `yaml:"defaults"`
	Button               *profile `yaml:"button"`
}

// DefaultConfig is the profile for keys missing from the scanner list.
func DefaultConfig() vens.ScanConfig {
	c := vens.DefaultScanConfig()
	c.Quality = vens.QualitySuperFine
	return c
}

func (p profile) apply(base vens.ScanConfig) (vens.ScanConfig, error) {
	c := base
	var errs []error
	var err error
	if p.Color != "" {
		if c.ColorMode, err = ParseColor(p.Color); err != nil {
			errs = append(errs, err)
		}
	}
	if p.DPI != "" {
		if c.Quality, err = ParseDPI(p.DPI); err != nil {
			errs = append(errs, err)
		}
	}
	if p.Paper != "" {
		if c.PaperSize, err = ParsePaper(p.Paper); err != nil {
			errs = append(errs, err)
		}
	}
	for _, b := range []struct {
		dst *bool
		v   *bool
	}{{&c.Duplex, p.Duplex}, {&c.BlankPageRemoval, p.BlankPageRemoval}, {&c.MultiFeed, p.MultiFeed}} {
		if b.v != nil {
			*b.dst = *b.v
		}
	}
	return c, errors.Join(errs...)
}

// Build implements scannerconfig.Driver.
func (d *Driver) Build(c scannerconfig.Common, decode func(any) error) (scanner.Device, error) {
	var e entry
	if err := decode(&e); err != nil {
		return nil, err
	}
	var errs []error
	ip, err := netip.ParseAddr(e.IP)
	switch {
	case err != nil || !ip.Is4():
		errs = append(errs, fmt.Errorf("ip: want an IPv4 address (a fixed DHCP reservation), got %q", e.IP))
	case d.ips[ip] != "":
		errs = append(errs, fmt.Errorf("ip %s is already used by ScanSnap %q (button events are told apart by IP)", ip, d.ips[ip]))
	}
	lookup := d.opts.LookupEnv
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	pw, err := scannerconfig.Secret(lookup, e.PasswordFile, e.PasswordEnv)
	if err != nil {
		errs = append(errs, err)
	} else if _, err := vens.ComputeIdentity(pw); err != nil {
		errs = append(errs, fmt.Errorf("password: %w", err))
	}
	defaults, err := e.Defaults.apply(DefaultConfig())
	if err != nil {
		errs = append(errs, fmt.Errorf("defaults: %w", err))
	}
	button := defaults
	if e.Button != nil {
		if button, err = e.Button.apply(DefaultConfig()); err != nil {
			errs = append(errs, fmt.Errorf("button: %w", err))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if d.hub == nil {
		d.hub = scansnap.NewNotifyHub(d.opts.NotifyPort, d.opts.Logger)
		if d.opts.AnnouncePort > 0 {
			d.hub.ListenAnnouncements(d.opts.AnnouncePort)
		}
	}
	d.ips[ip], d.ids[c.ID] = c.ID, ip.String()
	cfg := scansnap.Config{
		ScannerIP:   ip.String(),
		Password:    pw,
		JournalPath: scansnap.JournalPath(d.JournalDir(), c.ID),
		Hub:         d.hub,
		Logger:      d.opts.Logger.With("scanner", c.ID),
	}
	if d.opts.Tune != nil {
		d.opts.Tune(c.ID, &cfg)
	}
	return NewDevice(c.ID, c.Name, scansnap.New(cfg), defaults, button), nil
}

// Device is one ScanSnap.
type Device struct {
	id, name         string
	m                *scansnap.Manager
	defaults, button vens.ScanConfig
	buttons          chan scanner.ButtonEvent
	ran              atomic.Bool
}

// NewDevice wraps a manager. defaults prefill the UI form; button is the
// profile of the physical Scan button.
func NewDevice(id, name string, m *scansnap.Manager, defaults, button vens.ScanConfig) *Device {
	return &Device{id: id, name: name, m: m, defaults: defaults, button: button, buttons: make(chan scanner.ButtonEvent, 4)}
}

func (d *Device) ID() string         { return d.id }
func (d *Device) Name() string       { return d.name }
func (d *Device) Kind() scanner.Kind { return scanner.Feeder }

// Manager exposes the underlying manager (tests and the demo).
func (d *Device) Manager() *scansnap.Manager { return d.m }

// Run holds the scanner until ctx ends, forwarding button presses.
func (d *Device) Run(ctx context.Context) error {
	if !d.ran.CompareAndSwap(false, true) {
		return errors.New("scansnapdrv: device already ran")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(d.buttons)
		for ev := range d.m.Buttons() {
			select {
			case d.buttons <- scanner.ButtonEvent{At: ev.At, Seq: ev.Seq}:
			default:
			}
		}
	}()
	err := d.m.Run(ctx)
	<-done
	return err
}

func (d *Device) Buttons() <-chan scanner.ButtonEvent { return d.buttons }

// Release lets another computer use the scanner until TakeBack.
func (d *Device) Release(ctx context.Context) error {
	if err := d.m.Release(ctx); errors.Is(err, scansnap.ErrBusy) {
		return scanner.Explain(scanner.ErrBusy, d.name+" is scanning: release it after the scan.")
	} else if err != nil {
		return err
	}
	return nil
}

func (d *Device) TakeBack(ctx context.Context) error { return d.m.TakeBack(ctx) }
func (d *Device) Released() bool                     { return d.m.Status().State == scansnap.StateReleased }
func (d *Device) ButtonSettings() scanner.Settings   { return d.button }

var stateLabels = map[scansnap.State]string{
	scansnap.StateClaimed: "Ready", scansnap.StateScanning: "Scanning", scansnap.StateConnecting: "Connecting",
	scansnap.StateHeldByOther: "In use", scansnap.StateBadPassword: "Wrong password",
	scansnap.StateNotConnected: "Offline", scansnap.StateStopping: "Stopping", scansnap.StateStopped: "Stopped",
	scansnap.StateReleased: "Released",
}

func (d *Device) Status(context.Context) scanner.Status {
	st := d.m.Status()
	tone := scanner.ToneIdle
	switch st.State {
	case scansnap.StateClaimed:
		tone = scanner.ToneOK
	case scansnap.StateScanning, scansnap.StateConnecting:
		tone = scanner.ToneBusy
	case scansnap.StateHeldByOther, scansnap.StateBadPassword:
		tone = scanner.ToneBad
	}
	paper := "Empty"
	if st.Paper {
		paper = "Loaded"
	}
	facts := []scanner.Fact{{Label: "Paper", Value: paper}, {Label: "Wi-Fi", Signal: true, Level: st.WifiSignal}}
	if st.Holder != "" {
		facts = append(facts, scanner.Fact{Label: "Held by", Value: st.Holder})
	}
	if st.Firmware != "" {
		facts = append(facts, scanner.Fact{Label: "Firmware", Value: st.Firmware})
	}
	return scanner.Status{Tone: tone, Label: stateLabels[st.State], Detail: st.Detail, Facts: facts, Raw: st}
}

func (d *Device) Ready() error {
	st := d.m.Status()
	switch st.State {
	case scansnap.StateClaimed:
		return nil
	case scansnap.StateScanning:
		return scanner.Explain(scanner.ErrBusy, fmt.Sprintf("%s is already scanning.", d.name))
	case scansnap.StateHeldByOther:
		if st.Holder != "" {
			return scanner.Explain(scanner.ErrBusy, fmt.Sprintf("%s is in use by %s.", d.name, st.Holder))
		}
		return scanner.Explain(scanner.ErrBusy, d.name+" is in use by another computer.")
	case scansnap.StateBadPassword:
		return scanner.Explain(scanner.ErrNotReady, fmt.Sprintf("%s rejected the configured password.", d.name))
	case scansnap.StateReleased:
		return scanner.Explain(scanner.ErrNotReady, fmt.Sprintf("%s is released: take it back to scan.", d.name))
	}
	return scanner.Explain(scanner.ErrNotReady, fmt.Sprintf("%s is not connected yet (%s).", d.name, st.State))
}

func (d *Device) Form() []scanner.Field {
	color, dpi, paper := ProfileStrings(d.defaults)
	return []scanner.Field{
		{Name: "color", Label: "Colour", Type: scanner.Select, Value: color, Options: []scanner.Option{
			{Value: "auto", Label: "Auto"}, {Value: "color", Label: "Colour"}, {Value: "gray", Label: "Grey"}, {Value: "bw", Label: "Black & white"}}},
		{Name: "dpi", Label: "Resolution", Type: scanner.Select, Value: dpi, Options: []scanner.Option{
			{Value: "auto", Label: "Auto"}, {Value: "150", Label: "150 dpi"}, {Value: "200", Label: "200 dpi"}, {Value: "300", Label: "300 dpi"}}},
		{Name: "paper", Label: "Paper", Type: scanner.Select, Value: paper, Options: []scanner.Option{
			{Value: "auto", Label: "Auto-detect"}, {Value: "a4", Label: "A4"}, {Value: "a5", Label: "A5"},
			{Value: "business-card", Label: "Business card"}, {Value: "postcard", Label: "Postcard"}}},
		{Name: "duplex", Label: "Both sides", Type: scanner.Checkbox, Checked: d.defaults.Duplex},
		{Name: "blank", Label: "Skip blank pages", Type: scanner.Checkbox, Checked: d.defaults.BlankPageRemoval},
	}
}

func (d *Device) ParseForm(get func(string) string) (scanner.Settings, error) {
	c := d.defaults
	var errs []error
	var err error
	if c.ColorMode, err = ParseColor(get("color")); err != nil {
		errs = append(errs, err)
	}
	if c.Quality, err = ParseDPI(get("dpi")); err != nil {
		errs = append(errs, err)
	}
	if c.PaperSize, err = ParsePaper(get("paper")); err != nil {
		errs = append(errs, err)
	}
	c.Duplex, c.BlankPageRemoval = get("duplex") == "on", get("blank") == "on"
	return c, errors.Join(errs...)
}

func (d *Device) Scan(ctx context.Context, s scanner.Settings, onPage func(scanner.Page) error) error {
	cfg, ok := s.(vens.ScanConfig)
	if !ok {
		return fmt.Errorf("scansnapdrv: unexpected settings %T", s)
	}
	res := d.m.Scan(ctx, scansnap.ScanRequest{Config: cfg, OnPage: func(p scansnap.Page) error {
		return onPage(scanner.Page{Data: p.Data, Sheet: p.Sheet, Side: p.Side})
	}})
	return mapErr(res.Err)
}

// mapErr wraps protocol errors in the scanner sentinels.
func mapErr(err error) error {
	var se *vens.ScanError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, scansnap.ErrBusy):
		return fmt.Errorf("%w: %w", scanner.ErrBusy, err)
	case errors.Is(err, scansnap.ErrNotReady):
		return fmt.Errorf("%w: %w", scanner.ErrNotReady, err)
	case errors.As(err, &se):
		switch se.Kind {
		case vens.ScanErrNoPaper:
			return fmt.Errorf("%w: %w", scanner.ErrNoPaper, err)
		case vens.ScanErrPaperJam:
			return fmt.Errorf("%w: %w", scanner.ErrJam, err)
		case vens.ScanErrCoverOpen:
			return fmt.Errorf("%w: %w", scanner.ErrCoverOpen, err)
		case vens.ScanErrMultiFeed:
			return fmt.Errorf("%w: %w", scanner.ErrMultiFeed, err)
		}
	}
	return err
}

// ParseColor parses auto|color|gray|bw.
func ParseColor(s string) (vens.ColorMode, error) {
	switch strings.ToLower(s) {
	case "auto":
		return vens.ColorAuto, nil
	case "color", "colour":
		return vens.ColorColor, nil
	case "gray", "grey":
		return vens.ColorGray, nil
	case "bw":
		return vens.ColorBW, nil
	}
	return 0, fmt.Errorf("color: want auto|color|gray|bw, got %q", s)
}

// ParseDPI parses auto|150|200|300.
func ParseDPI(s string) (vens.Quality, error) {
	switch strings.ToLower(s) {
	case "auto":
		return vens.QualityAuto, nil
	case "150":
		return vens.QualityNormal, nil
	case "200":
		return vens.QualityFine, nil
	case "300":
		return vens.QualitySuperFine, nil
	}
	return 0, fmt.Errorf("dpi: want auto|150|200|300, got %q", s)
}

// ParsePaper parses auto|a4|a5|business-card|postcard.
func ParsePaper(s string) (vens.PaperSize, error) {
	switch strings.ToLower(s) {
	case "auto":
		return vens.PaperAuto, nil
	case "a4":
		return vens.PaperA4, nil
	case "a5":
		return vens.PaperA5, nil
	case "business-card":
		return vens.PaperBusinessCard, nil
	case "postcard":
		return vens.PaperPostcard, nil
	}
	return 0, fmt.Errorf("paper: want auto|a4|a5|business-card|postcard, got %q", s)
}

// ProfileStrings is the inverse of the parsers, for forms and logs.
func ProfileStrings(c vens.ScanConfig) (color, dpi, paper string) {
	color = map[vens.ColorMode]string{vens.ColorAuto: "auto", vens.ColorColor: "color", vens.ColorGray: "gray", vens.ColorBW: "bw"}[c.ColorMode]
	dpi = map[vens.Quality]string{vens.QualityAuto: "auto", vens.QualityNormal: "150", vens.QualityFine: "200", vens.QualitySuperFine: "300"}[c.Quality]
	paper = map[vens.PaperSize]string{vens.PaperAuto: "auto", vens.PaperA4: "a4", vens.PaperA5: "a5", vens.PaperBusinessCard: "business-card", vens.PaperPostcard: "postcard"}[c.PaperSize]
	return
}
