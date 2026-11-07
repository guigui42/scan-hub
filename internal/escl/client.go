// Package escl is a minimal eSCL (AirScan) client for a flatbed scanner
// such as the Epson ET-2750, with TLS pinning for its self-signed cert.
package escl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	// ErrBusy means a scan is already running or the device is busy.
	ErrBusy = errors.New("scanner busy")
	// ErrPinMismatch means the device's TLS key does not match the pin.
	ErrPinMismatch = errors.New("TLS pin mismatch")
)

const (
	maxXMLBytes = 1 << 20
	// MaxDocumentBytes caps one returned image.
	MaxDocumentBytes = 64 << 20
	maxDocuments     = 50
)

// Options configures a Client.
type Options struct {
	BaseURL  string // e.g. https://192.168.1.60 (the eSCL root is /eSCL)
	TLSPin   string // hex SHA-256 of the leaf certificate or its SPKI (colons allowed)
	Insecure bool   // skip verification entirely (explicit fallback only)
	Logger   *slog.Logger
}

// Client talks to one eSCL scanner. Scans are serialized.
type Client struct {
	base *url.URL
	hc   *http.Client
	log  *slog.Logger
	mu   sync.Mutex
}

// New builds a Client. HTTPS requires a pin unless Insecure is set.
func New(o Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(o.BaseURL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("escl: invalid base URL %q (want https://host[:port])", o.BaseURL)
	}
	u.Path = ""
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.MaxIdleConnsPerHost = 1
	if u.Scheme == "https" {
		switch {
		case o.TLSPin != "":
			pin, err := parsePin(o.TLSPin)
			if err != nil {
				return nil, err
			}
			tr.TLSClientConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
				// Self-signed device cert: the pin check below is the verification.
				InsecureSkipVerify: true,
				VerifyConnection: func(cs tls.ConnectionState) error {
					if len(cs.PeerCertificates) == 0 || !pinMatches(cs.PeerCertificates[0], pin) {
						return ErrPinMismatch
					}
					return nil
				},
			}
		case o.Insecure:
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
		default:
			return nil, errors.New("escl: HTTPS needs a TLS pin (or explicit insecure mode)")
		}
	}
	return &Client{
		base: u,
		hc: &http.Client{
			Transport: tr,
			// Never follow redirects off the configured device.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log: o.Logger.With("component", "escl", "host", u.Host),
	}, nil
}

func parsePin(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("escl: TLS pin must be a hex SHA-256 (cert or SPKI)")
	}
	return b, nil
}

func pinMatches(c *x509.Certificate, pin []byte) bool {
	certSum := sha256.Sum256(c.Raw)
	spkiSum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return bytes.Equal(certSum[:], pin) || bytes.Equal(spkiSum[:], pin)
}

// SPKIPin returns the hex SHA-256 of a certificate's SubjectPublicKeyInfo.
func SPKIPin(c *x509.Certificate) string {
	s := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(s[:])
}

// --- capabilities and status ---

// Capabilities is the subset of ScannerCapabilities scan-hub uses.
type Capabilities struct {
	Version      string
	MakeAndModel string
	MaxWidth     int // 1/300 inch
	MaxHeight    int // 1/300 inch
	ColorModes   []string
	Formats      []string
	Resolutions  []int
}

type capsXML struct {
	Version      string `xml:"Version"`
	MakeAndModel string `xml:"MakeAndModel"`
	Platen       struct {
		Caps struct {
			MaxWidth  int `xml:"MaxWidth"`
			MaxHeight int `xml:"MaxHeight"`
			Profiles  []struct {
				ColorModes  []string `xml:"ColorModes>ColorMode"`
				Formats     []string `xml:"DocumentFormats>DocumentFormat"`
				Resolutions []struct {
					X int `xml:"XResolution"`
					Y int `xml:"YResolution"`
				} `xml:"SupportedResolutions>DiscreteResolutions>DiscreteResolution"`
			} `xml:"SettingProfiles>SettingProfile"`
		} `xml:"PlatenInputCaps"`
	} `xml:"Platen"`
}

// Capabilities fetches /eSCL/ScannerCapabilities.
func (c *Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	var x capsXML
	if err := c.getXML(ctx, "/eSCL/ScannerCapabilities", &x); err != nil {
		return nil, err
	}
	caps := &Capabilities{
		Version: x.Version, MakeAndModel: x.MakeAndModel,
		MaxWidth: x.Platen.Caps.MaxWidth, MaxHeight: x.Platen.Caps.MaxHeight,
	}
	for _, p := range x.Platen.Caps.Profiles {
		caps.ColorModes = append(caps.ColorModes, p.ColorModes...)
		caps.Formats = append(caps.Formats, p.Formats...)
		for _, r := range p.Resolutions {
			if r.X == r.Y {
				caps.Resolutions = append(caps.Resolutions, r.X)
			}
		}
	}
	return caps, nil
}

// Status is the device state ("Idle", "Processing", "Testing", "Stopped", "Down").
type Status struct {
	State string `xml:"State"`
}

// Status fetches /eSCL/ScannerStatus.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.getXML(ctx, "/eSCL/ScannerStatus", &s)
	return s, err
}

func (c *Client) getXML(ctx context.Context, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(path), nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("escl: GET %s: %s", path, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxXMLBytes+1))
	if err != nil {
		return err
	}
	if len(b) > maxXMLBytes {
		return fmt.Errorf("escl: GET %s: response too large", path)
	}
	return xml.Unmarshal(b, v)
}

func (c *Client) url(path string) string { return c.base.String() + path }

// --- scanning ---

// ColorMode values accepted by eSCL.
const (
	ColorRGB24     = "RGB24"
	ColorGrayscale = "Grayscale8"
	ColorBW        = "BlackAndWhite1"
)

// Region is a scan area in 1/300 inch.
type Region struct{ Width, Height int }

// Common regions.
var (
	RegionA4     = Region{2480, 3508}
	RegionLetter = Region{2550, 3300}
)

// ScanSettings for one platen pass.
type ScanSettings struct {
	ColorMode string // ColorRGB24, ColorGrayscale, ColorBW
	DPI       int
	Region    Region // zero = full platen (from capabilities)
	Intent    string // default "Document"
}

// Document is one returned image.
type Document struct {
	ContentType string
	Data        []byte
}

const settingsTmpl = `<?xml version="1.0" encoding="UTF-8"?>
<scan:ScanSettings xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">
  <pwg:Version>2.63</pwg:Version>
  <scan:Intent>%s</scan:Intent>
  <pwg:ScanRegions>
    <pwg:ScanRegion>
      <pwg:ContentRegionUnits>escl:ThreeHundredthsOfInches</pwg:ContentRegionUnits>
      <pwg:XOffset>0</pwg:XOffset>
      <pwg:YOffset>0</pwg:YOffset>
      <pwg:Width>%d</pwg:Width>
      <pwg:Height>%d</pwg:Height>
    </pwg:ScanRegion>
  </pwg:ScanRegions>
  <pwg:InputSource>Platen</pwg:InputSource>
  <scan:ColorMode>%s</scan:ColorMode>
  <scan:XResolution>%d</scan:XResolution>
  <scan:YResolution>%d</scan:YResolution>
  <pwg:DocumentFormat>image/jpeg</pwg:DocumentFormat>
  <scan:DocumentFormatExt>image/jpeg</scan:DocumentFormatExt>
</scan:ScanSettings>
`

var intents = map[string]bool{"Document": true, "TextAndGraphic": true, "Photo": true, "Preview": true}

// Scan runs one platen pass and returns its documents (normally one JPEG).
// On failure or cancellation the job is deleted on the device.
func (c *Client) Scan(ctx context.Context, s ScanSettings) ([]Document, error) {
	if !c.mu.TryLock() {
		return nil, ErrBusy
	}
	defer c.mu.Unlock()

	switch s.ColorMode {
	case ColorRGB24, ColorGrayscale, ColorBW:
	default:
		return nil, fmt.Errorf("escl: unsupported colour mode %q", s.ColorMode)
	}
	if s.DPI <= 0 || s.DPI > 1200 {
		return nil, fmt.Errorf("escl: unsupported resolution %d", s.DPI)
	}
	if s.Intent == "" {
		s.Intent = "Document"
	}
	if !intents[s.Intent] {
		return nil, fmt.Errorf("escl: unsupported intent %q", s.Intent)
	}
	if s.Region == (Region{}) {
		caps, err := c.Capabilities(ctx)
		if err != nil {
			return nil, err
		}
		s.Region = Region{caps.MaxWidth, caps.MaxHeight}
	}
	if s.Region.Width <= 0 || s.Region.Height <= 0 {
		return nil, fmt.Errorf("escl: invalid region %+v", s.Region)
	}
	body := fmt.Sprintf(settingsTmpl, s.Intent, s.Region.Width, s.Region.Height, s.ColorMode, s.DPI, s.DPI)

	jobPath, err := c.createJob(ctx, body)
	if err != nil {
		return nil, err
	}
	log := c.log.With("job", jobPath)
	log.Info("scan job created", "dpi", s.DPI, "color", s.ColorMode, "width", s.Region.Width, "height", s.Region.Height)

	docs, err := c.fetchDocuments(ctx, jobPath)
	if err != nil {
		c.deleteJob(jobPath)
		return nil, err
	}
	log.Info("scan job finished", "documents", len(docs))
	return docs, nil
}

func (c *Client) createJob(ctx context.Context, body string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.url("/eSCL/ScanJobs"), strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/xml")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxXMLBytes))
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
	case http.StatusServiceUnavailable, http.StatusConflict:
		return "", ErrBusy
	default:
		return "", fmt.Errorf("escl: create job: %s", resp.Status)
	}
	return jobPath(resp.Header.Get("Location"))
}

// jobPath reduces the job Location to a path on our own base URL: devices
// often answer with an http:// or hostname URL, which is never followed.
func jobPath(loc string) (string, error) {
	u, err := url.Parse(loc)
	if err != nil || loc == "" {
		return "", fmt.Errorf("escl: bad job Location %q", loc)
	}
	p := strings.TrimRight(u.Path, "/")
	id := strings.TrimPrefix(p, "/eSCL/ScanJobs/")
	if id == p || id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return "", fmt.Errorf("escl: unexpected job Location %q", loc)
	}
	return p, nil
}

func (c *Client) fetchDocuments(ctx context.Context, jobPath string) ([]Document, error) {
	var docs []Document
	for range maxDocuments {
		doc, more, err := c.nextDocument(ctx, jobPath)
		if err != nil {
			return docs, err
		}
		if !more {
			if len(docs) == 0 {
				return nil, errors.New("escl: job finished without a document")
			}
			return docs, nil
		}
		docs = append(docs, doc)
	}
	return docs, fmt.Errorf("escl: more than %d documents", maxDocuments)
}

// nextDocument returns more=false on 404/410 (job complete). 503 means the
// image is not ready yet and is retried for up to 3 minutes.
func (c *Client) nextDocument(ctx context.Context, jobPath string) (Document, bool, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		doc, status, err := c.getDocument(ctx, jobPath)
		if err != nil {
			return Document{}, false, err
		}
		switch status {
		case http.StatusOK:
			return doc, true, nil
		case http.StatusNotFound, http.StatusGone:
			return Document{}, false, nil
		case http.StatusServiceUnavailable:
			if time.Now().After(deadline) {
				return Document{}, false, errors.New("escl: document not ready in time")
			}
			select {
			case <-ctx.Done():
				return Document{}, false, ctx.Err()
			case <-time.After(time.Second):
			}
		default:
			return Document{}, false, fmt.Errorf("escl: NextDocument: HTTP %d", status)
		}
	}
}

func (c *Client) getDocument(ctx context.Context, jobPath string) (Document, int, error) {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, c.url(jobPath+"/NextDocument"), nil)
	if err != nil {
		return Document{}, 0, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Document{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxXMLBytes))
		return Document{}, resp.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxDocumentBytes+1))
	if err != nil {
		return Document{}, 0, err
	}
	if len(data) > MaxDocumentBytes {
		return Document{}, 0, fmt.Errorf("escl: document exceeds %d bytes", MaxDocumentBytes)
	}
	return Document{ContentType: resp.Header.Get("Content-Type"), Data: data}, http.StatusOK, nil
}

// deleteJob cancels a job best-effort, independent of the caller's ctx.
func (c *Client) deleteJob(jobPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url(jobPath), nil)
	if err != nil {
		return
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.log.Warn("delete job", "job", jobPath, "err", err)
		return
	}
	resp.Body.Close()
	c.log.Info("scan job deleted", "job", jobPath, "status", resp.StatusCode)
}
