// Package config loads scan-hub settings from SCANHUB_* environment
// variables. Secrets may also be given as <NAME>_FILE paths.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/guigui42/scan-hub/internal/vens"
)

// Config is the full runtime configuration.
type Config struct {
	DataDir      string
	ListenAddr   string
	LogLevel     slog.Level
	AllowedHosts []string // Host headers the web UI answers to

	SessionMaxAge   time.Duration // abandoned sessions are deleted after this
	SessionMaxPages int
	MinFreeMB       int64

	ScanSnapIP       string // empty disables the ScanSnap
	ScanSnapPassword string `json:"-"`

	Button vens.ScanConfig // physical Scan button profile

	PaperlessURL   string // empty disables uploads
	PaperlessToken string `json:"-"`
	PaperlessTags  []int
	OutboxMaxMB    int64

	EpsonURL      string // empty disables the Epson
	EpsonTLSPin   string
	EpsonInsecure bool
	Epson         EpsonProfile // default flatbed settings
}

// EpsonProfile is the default eSCL scan for the flatbed.
type EpsonProfile struct {
	ColorMode string // escl.ColorRGB24 / ColorGrayscale / ColorBW
	DPI       int
	Region    string // a4, letter, max
}

// Load reads the environment.
func Load() (Config, error) { return load(os.LookupEnv) }

func load(lookup func(string) (string, bool)) (Config, error) {
	get := func(k, def string) string {
		if v, ok := lookup(k); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	var errs []error
	c := Config{
		DataDir:    get("SCANHUB_DATA_DIR", "/data"),
		ListenAddr: get("SCANHUB_LISTEN_ADDR", "127.0.0.1:8097"),
		ScanSnapIP: get("SCANHUB_SCANSNAP_IP", ""),
	}
	var e0 error
	if c.SessionMaxAge, e0 = time.ParseDuration(get("SCANHUB_SESSION_MAX_AGE", "24h")); e0 != nil || c.SessionMaxAge < time.Hour {
		errs = append(errs, fmt.Errorf("SCANHUB_SESSION_MAX_AGE: want a duration of at least 1h, got %q", get("SCANHUB_SESSION_MAX_AGE", "")))
	}
	if c.SessionMaxPages, e0 = strconv.Atoi(get("SCANHUB_SESSION_MAX_PAGES", "200")); e0 != nil || c.SessionMaxPages < 1 || c.SessionMaxPages > 1000 {
		errs = append(errs, errors.New("SCANHUB_SESSION_MAX_PAGES: want 1..1000"))
	}
	if c.MinFreeMB, e0 = strconv.ParseInt(get("SCANHUB_MIN_FREE_MB", "512"), 10, 64); e0 != nil || c.MinFreeMB < 0 {
		errs = append(errs, errors.New("SCANHUB_MIN_FREE_MB: want a non-negative integer"))
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("SCANHUB_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("SCANHUB_LOG_LEVEL: %w", err))
	}
	if host, port, err := net.SplitHostPort(c.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("SCANHUB_LISTEN_ADDR: %w", err))
	} else {
		c.AllowedHosts = []string{c.ListenAddr, "localhost:" + port, "127.0.0.1:" + port}
		if host == "" || host == "0.0.0.0" || host == "::" {
			c.AllowedHosts = c.AllowedHosts[1:]
		}
		for _, h := range strings.Split(get("SCANHUB_ALLOWED_HOSTS", ""), ",") {
			if h = strings.TrimSpace(h); h != "" {
				c.AllowedHosts = append(c.AllowedHosts, h)
			}
		}
		slices.Sort(c.AllowedHosts)
		c.AllowedHosts = slices.Compact(c.AllowedHosts)
	}
	if !filepath.IsAbs(c.DataDir) {
		errs = append(errs, fmt.Errorf("SCANHUB_DATA_DIR must be absolute: %q", c.DataDir))
	}

	pw, err := secret(lookup, "SCANHUB_SCANSNAP_PASSWORD")
	if err != nil {
		errs = append(errs, err)
	}
	c.ScanSnapPassword = pw
	if c.ScanSnapIP != "" {
		if ip := net.ParseIP(c.ScanSnapIP); ip == nil || ip.To4() == nil {
			errs = append(errs, fmt.Errorf("SCANHUB_SCANSNAP_IP must be an IPv4 address: %q", c.ScanSnapIP))
		}
		if c.ScanSnapPassword == "" {
			errs = append(errs, errors.New("SCANHUB_SCANSNAP_PASSWORD (or _FILE) is required when SCANHUB_SCANSNAP_IP is set"))
		}
	}

	b := vens.DefaultScanConfig()
	b.Quality = vens.QualitySuperFine
	var e error
	if b.ColorMode, e = parseColor(get("SCANHUB_BUTTON_COLOR", "auto")); e != nil {
		errs = append(errs, e)
	}
	if b.Quality, e = parseDPI(get("SCANHUB_BUTTON_DPI", "300")); e != nil {
		errs = append(errs, e)
	}
	if b.PaperSize, e = parsePaper(get("SCANHUB_BUTTON_PAPER", "auto")); e != nil {
		errs = append(errs, e)
	}
	if b.Duplex, e = parseBool("SCANHUB_BUTTON_DUPLEX", get("SCANHUB_BUTTON_DUPLEX", "true")); e != nil {
		errs = append(errs, e)
	}
	if b.BlankPageRemoval, e = parseBool("SCANHUB_BUTTON_BLANK_PAGE_REMOVAL", get("SCANHUB_BUTTON_BLANK_PAGE_REMOVAL", "true")); e != nil {
		errs = append(errs, e)
	}
	if b.MultiFeed, e = parseBool("SCANHUB_BUTTON_MULTIFEED", get("SCANHUB_BUTTON_MULTIFEED", "true")); e != nil {
		errs = append(errs, e)
	}
	c.Button = b

	c.PaperlessURL = get("SCANHUB_PAPERLESS_URL", "")
	if c.PaperlessToken, e = secret(lookup, "SCANHUB_PAPERLESS_TOKEN"); e != nil {
		errs = append(errs, e)
	}
	if c.PaperlessURL != "" && c.PaperlessToken == "" {
		errs = append(errs, errors.New("SCANHUB_PAPERLESS_TOKEN (or _FILE) is required when SCANHUB_PAPERLESS_URL is set"))
	}
	if v := get("SCANHUB_PAPERLESS_TAGS", ""); v != "" {
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n <= 0 {
				errs = append(errs, fmt.Errorf("SCANHUB_PAPERLESS_TAGS: want comma-separated tag IDs, got %q", v))
				break
			}
			c.PaperlessTags = append(c.PaperlessTags, n)
		}
	}
	if c.OutboxMaxMB, e = strconv.ParseInt(get("SCANHUB_OUTBOX_MAX_MB", "2048"), 10, 64); e != nil || c.OutboxMaxMB < 1 {
		errs = append(errs, errors.New("SCANHUB_OUTBOX_MAX_MB: want a positive integer"))
	}

	c.EpsonURL = get("SCANHUB_EPSON_URL", "")
	c.EpsonTLSPin = get("SCANHUB_EPSON_TLS_PIN", "")
	if c.EpsonInsecure, e = parseBool("SCANHUB_EPSON_TLS_INSECURE", get("SCANHUB_EPSON_TLS_INSECURE", "false")); e != nil {
		errs = append(errs, e)
	}
	if strings.HasPrefix(c.EpsonURL, "https://") && c.EpsonTLSPin == "" && !c.EpsonInsecure {
		errs = append(errs, errors.New("SCANHUB_EPSON_TLS_PIN is required for an https SCANHUB_EPSON_URL (or set SCANHUB_EPSON_TLS_INSECURE=true)"))
	}
	if c.Epson, e = parseEpsonProfile(get("SCANHUB_EPSON_COLOR", "color"), get("SCANHUB_EPSON_DPI", "300"), get("SCANHUB_EPSON_REGION", "a4")); e != nil {
		errs = append(errs, e)
	}
	return c, errors.Join(errs...)
}

// secret reads NAME, or the file named by NAME_FILE (not both).
func secret(lookup func(string) (string, bool), name string) (string, error) {
	v, hasV := lookup(name)
	f, hasF := lookup(name + "_FILE")
	switch {
	case hasV && v != "" && hasF && f != "":
		return "", fmt.Errorf("set only one of %s and %s_FILE", name, name)
	case hasF && f != "":
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(v), nil
}

// LogValue keeps secrets out of logs.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("data_dir", c.DataDir),
		slog.String("listen_addr", c.ListenAddr),
		slog.Any("allowed_hosts", c.AllowedHosts),
		slog.String("log_level", c.LogLevel.String()),
		slog.Duration("session_max_age", c.SessionMaxAge),
		slog.Int("session_max_pages", c.SessionMaxPages),
		slog.Int64("min_free_mb", c.MinFreeMB),
		slog.String("scansnap_ip", c.ScanSnapIP),
		slog.Bool("scansnap_password_set", c.ScanSnapPassword != ""),
		slog.Int("button_color", int(c.Button.ColorMode)),
		slog.Int("button_quality", int(c.Button.Quality)),
		slog.Bool("button_duplex", c.Button.Duplex),
		slog.Int("button_paper", int(c.Button.PaperSize)),
		slog.Bool("button_blank_page_removal", c.Button.BlankPageRemoval),
		slog.String("paperless_url", c.PaperlessURL),
		slog.Bool("paperless_token_set", c.PaperlessToken != ""),
		slog.Any("paperless_tags", c.PaperlessTags),
		slog.Int64("outbox_max_mb", c.OutboxMaxMB),
		slog.String("epson_url", c.EpsonURL),
		slog.Bool("epson_tls_pinned", c.EpsonTLSPin != ""),
		slog.Bool("epson_tls_insecure", c.EpsonInsecure),
		slog.String("epson_profile", fmt.Sprintf("%s/%d/%s", c.Epson.ColorMode, c.Epson.DPI, c.Epson.Region)),
	)
}

func parseColor(s string) (vens.ColorMode, error) {
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
	return 0, fmt.Errorf("SCANHUB_BUTTON_COLOR: want auto|color|gray|bw, got %q", s)
}

func parseDPI(s string) (vens.Quality, error) {
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
	return 0, fmt.Errorf("SCANHUB_BUTTON_DPI: want auto|150|200|300, got %q", s)
}

func parsePaper(s string) (vens.PaperSize, error) {
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
	return 0, fmt.Errorf("SCANHUB_BUTTON_PAPER: want auto|a4|a5|business-card|postcard, got %q", s)
}

func parseBool(name, s string) (bool, error) {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return b, nil
}

// parseEpsonProfile validates user-facing Epson settings (also used by the
// CLI and UI).
func parseEpsonProfile(color, dpi, region string) (EpsonProfile, error) {
	var p EpsonProfile
	var errs []error
	switch strings.ToLower(color) {
	case "color", "colour":
		p.ColorMode = "RGB24"
	case "gray", "grey":
		p.ColorMode = "Grayscale8"
	case "bw":
		p.ColorMode = "BlackAndWhite1"
	default:
		errs = append(errs, fmt.Errorf("SCANHUB_EPSON_COLOR: want color|gray|bw, got %q", color))
	}
	switch dpi {
	case "100", "200", "300", "600":
		p.DPI, _ = strconv.Atoi(dpi)
	default:
		errs = append(errs, fmt.Errorf("SCANHUB_EPSON_DPI: want 100|200|300|600, got %q", dpi))
	}
	switch r := strings.ToLower(region); r {
	case "a4", "letter", "max":
		p.Region = r
	default:
		errs = append(errs, fmt.Errorf("SCANHUB_EPSON_REGION: want a4|letter|max, got %q", region))
	}
	return p, errors.Join(errs...)
}

// ParseEpsonProfile is parseEpsonProfile for callers outside the package.
func ParseEpsonProfile(color, dpi, region string) (EpsonProfile, error) {
	return parseEpsonProfile(color, dpi, region)
}

// SessionOptions returns the session store limits.
func (c Config) SessionOptions() (maxPages int, minFreeBytes int64) {
	return c.SessionMaxPages, c.MinFreeMB << 20
}

// ParseScanSnapProfile validates user-facing ScanSnap settings (UI forms).
func ParseScanSnapProfile(color, dpi, paper string, duplex, blankRemoval bool) (vens.ScanConfig, error) {
	c := vens.DefaultScanConfig()
	var errs []error
	var e error
	if c.ColorMode, e = parseColor(color); e != nil {
		errs = append(errs, e)
	}
	if c.Quality, e = parseDPI(dpi); e != nil {
		errs = append(errs, e)
	}
	if c.PaperSize, e = parsePaper(paper); e != nil {
		errs = append(errs, e)
	}
	c.Duplex, c.BlankPageRemoval = duplex, blankRemoval
	return c, errors.Join(errs...)
}

// ScanSnapProfileStrings is the inverse of ParseScanSnapProfile, for forms.
func ScanSnapProfileStrings(c vens.ScanConfig) (color, dpi, paper string) {
	color = map[vens.ColorMode]string{vens.ColorAuto: "auto", vens.ColorColor: "color", vens.ColorGray: "gray", vens.ColorBW: "bw"}[c.ColorMode]
	dpi = map[vens.Quality]string{vens.QualityAuto: "auto", vens.QualityNormal: "150", vens.QualityFine: "200", vens.QualitySuperFine: "300"}[c.Quality]
	paper = map[vens.PaperSize]string{vens.PaperAuto: "auto", vens.PaperA4: "a4", vens.PaperA5: "a5", vens.PaperBusinessCard: "business-card", vens.PaperPostcard: "postcard"}[c.PaperSize]
	return
}

// EpsonProfileStrings returns the form values of an Epson profile.
func EpsonProfileStrings(p EpsonProfile) (color, dpi, region string) {
	color = map[string]string{"RGB24": "color", "Grayscale8": "gray", "BlackAndWhite1": "bw"}[p.ColorMode]
	return color, strconv.Itoa(p.DPI), p.Region
}
