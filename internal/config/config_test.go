package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guigui42/scan-hub/internal/vens"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/data" || c.ListenAddr != "127.0.0.1:8097" || c.ScanSnapIP != "" {
		t.Fatalf("%+v", c)
	}
	if c.Button.Quality != vens.QualitySuperFine || !c.Button.Duplex || c.Button.ColorMode != vens.ColorAuto || !c.Button.BlankPageRemoval {
		t.Fatalf("button profile %+v", c.Button)
	}
}

func TestPasswordRequiredAndFile(t *testing.T) {
	if _, err := load(env(map[string]string{"SCANHUB_SCANSNAP_IP": "192.168.1.50"})); err == nil {
		t.Fatal("missing password accepted")
	}
	f := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(f, []byte("s3cr\n"), 0o600)
	c, err := load(env(map[string]string{"SCANHUB_SCANSNAP_IP": "192.168.1.50", "SCANHUB_SCANSNAP_PASSWORD_FILE": f}))
	if err != nil || c.ScanSnapPassword != "s3cr" {
		t.Fatalf("%v %q", err, c.ScanSnapPassword)
	}
	if _, err := load(env(map[string]string{"SCANHUB_SCANSNAP_PASSWORD": "a", "SCANHUB_SCANSNAP_PASSWORD_FILE": f})); err == nil {
		t.Fatal("both forms accepted")
	}
}

func TestInvalidValues(t *testing.T) {
	_, err := load(env(map[string]string{
		"SCANHUB_BUTTON_DPI": "600", "SCANHUB_BUTTON_COLOR": "pink", "SCANHUB_DATA_DIR": "rel",
		"SCANHUB_SCANSNAP_IP": "scanner.local", "SCANHUB_SCANSNAP_PASSWORD": "x",
	}))
	for _, want := range []string{"DPI", "COLOR", "DATA_DIR", "SCANSNAP_IP"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %s in %v", want, err)
		}
	}
}

func TestLogValueRedactsPassword(t *testing.T) {
	c, _ := load(env(map[string]string{"SCANHUB_SCANSNAP_IP": "192.168.1.50", "SCANHUB_SCANSNAP_PASSWORD": "topsecret"}))
	var b bytes.Buffer
	slog.New(slog.NewTextHandler(&b, nil)).Info("cfg", "config", c)
	if strings.Contains(b.String(), "topsecret") {
		t.Fatal("password logged")
	}
}

func TestEpson(t *testing.T) {
	if _, err := load(env(map[string]string{"SCANHUB_EPSON_URL": "https://192.168.1.60"})); err == nil {
		t.Fatal("https without pin accepted")
	}
	c, err := load(env(map[string]string{"SCANHUB_EPSON_URL": "https://192.168.1.60", "SCANHUB_EPSON_TLS_PIN": "ab", "SCANHUB_EPSON_DPI": "600", "SCANHUB_EPSON_COLOR": "gray"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Epson != (EpsonProfile{ColorMode: "Grayscale8", DPI: 600, Region: "a4"}) {
		t.Fatalf("%+v", c.Epson)
	}
	if _, err := load(env(map[string]string{"SCANHUB_EPSON_REGION": "a3"})); err == nil {
		t.Fatal("bad region accepted")
	}
}

func TestPaperless(t *testing.T) {
	if _, err := load(env(map[string]string{"SCANHUB_PAPERLESS_URL": "https://paperless.example"})); err == nil {
		t.Fatal("missing token accepted")
	}
	c, err := load(env(map[string]string{"SCANHUB_PAPERLESS_URL": "https://paperless.example", "SCANHUB_PAPERLESS_TOKEN": "tok-abc", "SCANHUB_PAPERLESS_TAGS": "3, 12"}))
	if err != nil || len(c.PaperlessTags) != 2 || c.PaperlessTags[1] != 12 || c.OutboxMaxMB != 2048 {
		t.Fatalf("%+v %v", c, err)
	}
	var b bytes.Buffer
	slog.New(slog.NewTextHandler(&b, nil)).Info("cfg", "config", c)
	if strings.Contains(b.String(), "tok-abc") {
		t.Fatal("token logged")
	}
	if _, err := load(env(map[string]string{"SCANHUB_PAPERLESS_TAGS": "a,b"})); err == nil {
		t.Fatal("bad tags accepted")
	}
}
