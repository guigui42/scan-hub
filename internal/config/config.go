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

	ScannersFile string // YAML scanner list; empty means no scanners

	PaperlessURL   string // empty disables uploads
	PaperlessToken string `json:"-"`
	PaperlessTags  []int
	OutboxMaxMB    int64
}

// legacyScannerVars moved to the scanner list in the multi-scanner release.
var legacyScannerVars = []string{
	"SCANHUB_SCANSNAP_IP", "SCANHUB_SCANSNAP_PASSWORD_FILE",
	"SCANHUB_BUTTON_COLOR", "SCANHUB_BUTTON_DPI", "SCANHUB_BUTTON_DUPLEX", "SCANHUB_BUTTON_PAPER",
	"SCANHUB_BUTTON_BLANK_PAGE_REMOVAL", "SCANHUB_BUTTON_MULTIFEED",
	"SCANHUB_EPSON_URL", "SCANHUB_EPSON_TLS_PIN", "SCANHUB_EPSON_TLS_INSECURE",
	"SCANHUB_EPSON_COLOR", "SCANHUB_EPSON_DPI", "SCANHUB_EPSON_REGION",
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
		DataDir:      get("SCANHUB_DATA_DIR", "/data"),
		ListenAddr:   get("SCANHUB_LISTEN_ADDR", "127.0.0.1:8097"),
		ScannersFile: get("SCANHUB_SCANNERS_FILE", ""),
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
	if c.ScannersFile != "" && !filepath.IsAbs(c.ScannersFile) {
		errs = append(errs, fmt.Errorf("SCANHUB_SCANNERS_FILE must be absolute: %q", c.ScannersFile))
	}
	var legacy []string
	for _, k := range legacyScannerVars {
		if get(k, "") != "" {
			legacy = append(legacy, k)
		}
	}
	if len(legacy) > 0 {
		errs = append(errs, fmt.Errorf("%s: scanners are now configured in the file named by SCANHUB_SCANNERS_FILE (see scanners.example.yaml)", strings.Join(legacy, ", ")))
	}

	c.PaperlessURL = get("SCANHUB_PAPERLESS_URL", "")
	var e error
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
		slog.String("scanners_file", c.ScannersFile),
		slog.String("paperless_url", c.PaperlessURL),
		slog.Bool("paperless_token_set", c.PaperlessToken != ""),
		slog.Any("paperless_tags", c.PaperlessTags),
		slog.Int64("outbox_max_mb", c.OutboxMaxMB),
	)
}

// SessionOptions returns the session store limits.
func (c Config) SessionOptions() (maxPages int, minFreeBytes int64) {
	return c.SessionMaxPages, c.MinFreeMB << 20
}
