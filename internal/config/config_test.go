package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/data" || c.ListenAddr != "127.0.0.1:8097" || c.ScannersFile != "" {
		t.Fatalf("%+v", c)
	}
}

func TestInvalidValues(t *testing.T) {
	_, err := load(env(map[string]string{"SCANHUB_DATA_DIR": "rel", "SCANHUB_SCANNERS_FILE": "scanners.yaml"}))
	for _, want := range []string{"DATA_DIR", "SCANNERS_FILE"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %s in %v", want, err)
		}
	}
}

func TestLegacyScannerVarsRejected(t *testing.T) {
	_, err := load(env(map[string]string{"SCANHUB_SCANSNAP_IP": "192.168.1.50", "SCANHUB_EPSON_URL": "https://192.168.1.60", "SCANHUB_EPSON_DPI": ""}))
	if err == nil || !strings.Contains(err.Error(), "SCANHUB_SCANSNAP_IP, SCANHUB_EPSON_URL: scanners are now configured") {
		t.Fatalf("got %v", err)
	}
	// The password may still live in an environment variable named by the list.
	if _, err := load(env(map[string]string{"SCANHUB_SCANSNAP_PASSWORD": "x", "SCANHUB_SCANNERS_FILE": "/etc/scan-hub/scanners.yaml"})); err != nil {
		t.Fatal(err)
	}
}

func TestSecretFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(f, []byte("tok-abc\n"), 0o600)
	c, err := load(env(map[string]string{"SCANHUB_PAPERLESS_URL": "https://paperless.example", "SCANHUB_PAPERLESS_TOKEN_FILE": f}))
	if err != nil || c.PaperlessToken != "tok-abc" {
		t.Fatalf("%v %q", err, c.PaperlessToken)
	}
	if _, err := load(env(map[string]string{"SCANHUB_PAPERLESS_TOKEN": "a", "SCANHUB_PAPERLESS_TOKEN_FILE": f})); err == nil {
		t.Fatal("both forms accepted")
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
