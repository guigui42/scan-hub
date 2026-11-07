package escl_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/guigui42/scan-hub/internal/escl"
	"github.com/guigui42/scan-hub/internal/escl/escltest"
)

func client(t *testing.T, s *escltest.Server, pin string) *escl.Client {
	t.Helper()
	c, err := escl.New(escl.Options{BaseURL: s.URL, TLSPin: pin})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func spki(s *escltest.Server) string { return escl.SPKIPin(s.Certificate()) }

func TestPinning(t *testing.T) {
	s := escltest.Start()
	defer s.Close()
	ctx := context.Background()

	if _, err := client(t, s, spki(s)).Status(ctx); err != nil {
		t.Fatalf("SPKI pin: %v", err)
	}
	sum := sha256.Sum256(s.Certificate().Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	colon := strings.Join(parts, ":") // openssl -fingerprint format
	if _, err := client(t, s, colon).Status(ctx); err != nil {
		t.Fatalf("cert pin: %v", err)
	}
	wrong := strings.Repeat("ab", 32)
	if _, err := client(t, s, wrong).Status(ctx); !errors.Is(err, escl.ErrPinMismatch) {
		t.Fatalf("want pin mismatch, got %v", err)
	}
	if _, err := escl.New(escl.Options{BaseURL: s.URL}); err == nil {
		t.Fatal("HTTPS without pin accepted")
	}
	if _, err := escl.New(escl.Options{BaseURL: s.URL, Insecure: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := escl.New(escl.Options{BaseURL: s.URL + "/eSCL", TLSPin: spki(s)}); err == nil {
		t.Fatal("base URL with path accepted")
	}
}

func TestCapabilitiesFromRealDevice(t *testing.T) {
	s := escltest.Start()
	defer s.Close()
	caps, err := client(t, s, spki(s)).Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.MakeAndModel != "EPSON ET-2750 Series" || caps.MaxWidth != 2550 || caps.MaxHeight != 3510 {
		t.Fatalf("%+v", caps)
	}
	if strings.Join(caps.ColorModes, ",") != "BlackAndWhite1,Grayscale8,RGB24" {
		t.Fatalf("modes %v", caps.ColorModes)
	}
	if len(caps.Resolutions) != 5 || caps.Resolutions[2] != 300 {
		t.Fatalf("resolutions %v", caps.Resolutions)
	}
}

func TestScanOnePage(t *testing.T) {
	s := escltest.Start()
	defer s.Close()
	s.NotReady = 2
	docs, err := client(t, s, spki(s)).Scan(context.Background(), escl.ScanSettings{
		ColorMode: escl.ColorRGB24, DPI: 300, Region: escl.RegionA4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].ContentType != "image/jpeg" || len(docs[0].Data) < 100 {
		t.Fatalf("docs %d", len(docs))
	}
	body := s.LastSettings()
	for _, want := range []string{"<scan:ColorMode>RGB24<", "<scan:XResolution>300<", "<pwg:Width>2480<", "<pwg:Height>3508<", "<pwg:InputSource>Platen<", "image/jpeg"} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings missing %q:\n%s", want, body)
		}
	}
	if len(s.DeletedJobs()) != 0 {
		t.Fatal("successful job was deleted")
	}
}

func TestFullPlatenWhenNoRegion(t *testing.T) {
	s := escltest.Start()
	defer s.Close()
	if _, err := client(t, s, spki(s)).Scan(context.Background(), escl.ScanSettings{ColorMode: escl.ColorGrayscale, DPI: 150}); err != nil {
		t.Fatal(err)
	}
	if b := s.LastSettings(); !strings.Contains(b, "<pwg:Width>2550<") || !strings.Contains(b, "<pwg:Height>3510<") {
		t.Fatalf("not full platen:\n%s", b)
	}
}

func TestCancelDeletesJob(t *testing.T) {
	s := escltest.Start()
	defer s.Close()
	s.Hang = true
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client(t, s, spki(s)).Scan(ctx, escl.ScanSettings{ColorMode: escl.ColorRGB24, DPI: 300})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel not prompt")
	}
	if d := s.DeletedJobs(); len(d) != 1 {
		t.Fatalf("deleted %v", d)
	}
}

func TestBusyAndValidation(t *testing.T) {
	s := escltest.Start()
	defer s.Close()
	c := client(t, s, spki(s))
	s.Busy = true
	if _, err := c.Scan(context.Background(), escl.ScanSettings{ColorMode: escl.ColorRGB24, DPI: 300}); !errors.Is(err, escl.ErrBusy) {
		t.Fatalf("want busy, got %v", err)
	}
	for _, bad := range []escl.ScanSettings{
		{ColorMode: "CMYK", DPI: 300},
		{ColorMode: escl.ColorRGB24, DPI: 0},
		{ColorMode: escl.ColorRGB24, DPI: 300, Intent: "<x/>"},
	} {
		if _, err := c.Scan(context.Background(), bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
