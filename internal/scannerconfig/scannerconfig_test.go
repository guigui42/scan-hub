package scannerconfig_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guigui42/scan-hub/internal/escl/escltest"
	"github.com/guigui42/scan-hub/internal/scanner"
	"github.com/guigui42/scan-hub/internal/scanner/escldrv"
	"github.com/guigui42/scan-hub/internal/scanner/scansnapdrv"
	"github.com/guigui42/scan-hub/internal/scannerconfig"
	"github.com/guigui42/scan-hub/internal/vens"
)

func drivers(t *testing.T, env map[string]string) (map[string]scannerconfig.Driver, *scansnapdrv.Driver) {
	t.Helper()
	ss := scansnapdrv.New(scansnapdrv.Options{DataDir: t.TempDir(), LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	t.Cleanup(func() { ss.Close() })
	return map[string]scannerconfig.Driver{scansnapdrv.Type: ss, escldrv.Type: escldrv.New(escldrv.Options{})}, ss
}

func parse(t *testing.T, yaml string, env map[string]string) ([]scanner.Device, error) {
	t.Helper()
	d, _ := drivers(t, env)
	return scannerconfig.Parse([]byte(yaml), d)
}

func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error mentioning %q", parts)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Fatalf("error lacks %q:\n%v", p, err)
		}
	}
}

func TestFullList(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(pw, []byte("1234\n"), 0o600)
	devs, err := parse(t, `
# comments are fine
scanners:
  - id: feeder
    type: scansnap
    name: ScanSnap iX500
    ip: 192.168.1.50
    password_file: `+pw+`
    defaults: {color: gray, dpi: 200, duplex: false}
    button: {dpi: 300, multifeed: false}
  - id: feeder-2
    type: scansnap
    ip: 192.168.1.51
    password_env: SECOND_PW
  - id: glass
    type: escl
    name: Epson ET-2750
    url: https://192.168.1.60
    tls_pin: `+strings.Repeat("ab", 32)+`
    defaults: {dpi: 600, region: max}
  - id: office
    type: escl
    url: http://192.168.1.61
`, map[string]string{"SECOND_PW": "5678"})
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 4 {
		t.Fatalf("%d devices", len(devs))
	}
	f := devs[0]
	if f.ID() != "feeder" || f.Name() != "ScanSnap iX500" || f.Kind() != scanner.Feeder || devs[1].Name() != "feeder-2" {
		t.Fatalf("feeder: %s %s %s / %s", f.ID(), f.Name(), f.Kind(), devs[1].Name())
	}
	form := map[string]scanner.Field{}
	for _, fl := range f.Form() {
		form[fl.Name] = fl
	}
	// Omitted keys keep their defaults; an explicit false is kept.
	if form["color"].Value != "gray" || form["dpi"].Value != "200" || form["paper"].Value != "auto" ||
		form["duplex"].Checked || !form["blank"].Checked {
		t.Fatalf("form %+v", form)
	}
	btn := f.(scanner.Button).ButtonSettings().(vens.ScanConfig)
	if btn.Quality != vens.QualitySuperFine || btn.MultiFeed || btn.ColorMode != vens.ColorAuto || !btn.Duplex {
		t.Fatalf("button %+v", btn)
	}
	// Without a button section the button uses the defaults.
	if b2 := devs[1].(scanner.Button).ButtonSettings().(vens.ScanConfig); b2 != scansnapdrv.DefaultConfig() {
		t.Fatalf("button of feeder-2 %+v", b2)
	}
	g := devs[2]
	if g.Kind() != scanner.Flatbed || g.Name() != "Epson ET-2750" {
		t.Fatalf("glass %s %s", g.Kind(), g.Name())
	}
	for _, fl := range g.Form() {
		if fl.Name == "dpi" && fl.Value != "600" || fl.Name == "region" && fl.Value != "max" || fl.Name == "color" && fl.Value != "color" {
			t.Fatalf("glass field %+v", fl)
		}
	}
}

func TestEmptyAndMissing(t *testing.T) {
	for _, y := range []string{"", "scanners: []\n", "# nothing\n"} {
		if devs, err := parse(t, y, nil); err != nil || len(devs) != 0 {
			t.Fatalf("%q: %v %d", y, err, len(devs))
		}
	}
	d, _ := drivers(t, nil)
	if _, err := scannerconfig.Load(filepath.Join(t.TempDir(), "none.yaml"), d); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestStrictness(t *testing.T) {
	_, err := parse(t, "scanner:\n  - id: a\n", nil)
	wantErr(t, err, "scanner")

	_, err = parse(t, `scanners:
  - id: glass
    type: escl
    url: http://192.168.1.60
    defaults: {dpi: 300, regoin: a4}
`, nil)
	wantErr(t, err, "scanners[0] (glass)", "regoin")

	_, err = parse(t, `scanners:
  - id: feeder
    type: scansnap
    ip: 192.168.1.50
    password_env: PW
    button: {duplex: maybe}
`, map[string]string{"PW": "1"})
	wantErr(t, err, "scanners[0] (feeder)", "maybe")

	_, err = parse(t, `scanners:
  - id: glass
    type: escl
    url: http://192.168.1.60
    tls: true
`, nil)
	wantErr(t, err, "tls")
}

func TestCommonErrorsReportedTogether(t *testing.T) {
	_, err := parse(t, `scanners:
  - id: Bad_ID
    type: escl
    url: http://192.168.1.60
  - id: a
    type: twain
  - id: b
    type: escl
    url: http://192.168.1.60
  - id: b
    type: escl
    url: http://192.168.1.61
  - id: c
    type: escl
    name: "`+strings.Repeat("x", 61)+`"
    url: http://192.168.1.62
`, nil)
	wantErr(t, err, `scanners[0] (Bad_ID): id:`, `scanners[1] (a): type: want one of escl, scansnap, got "twain"`, `id "b" is used twice`, "scanners[4] (c): name")
}

func TestESCLValidation(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	for y, want := range map[string]string{
		"url: ftp://192.168.1.60":   "url: want http",
		"url: https://192.168.1.60": "exactly one of tls_pin",
		"url: https://192.168.1.60\n    tls_pin: " + pin + "\n    tls_insecure: true": "exactly one of tls_pin",
		"url: http://192.168.1.60\n    tls_pin: " + pin:                               "https urls only",
		"url: http://192.168.1.60\n    defaults: {dpi: 1200}":                         "dpi: want 100|200|300|600",
		"name: x": "url is required",
	} {
		_, err := parse(t, "scanners:\n  - id: g\n    type: escl\n    "+y+"\n", nil)
		wantErr(t, err, want)
	}
	if _, err := parse(t, "scanners:\n  - id: g\n    type: escl\n    url: https://192.168.1.60\n    tls_insecure: true\n", nil); err != nil {
		t.Fatal(err)
	}
}

func TestScanSnapValidation(t *testing.T) {
	env := map[string]string{"PW": "1234", "EMPTY": " "}
	for y, want := range map[string]string{
		"ip: scanner.example.com\n    password_env: PW": "ip: want an IPv4",
		"ip: 192.168.1.50":                                                  "password_file or password_env is required",
		"ip: 192.168.1.50\n    password_env: NOPE":                          "password_env: the variable is not set",
		"ip: 192.168.1.50\n    password_env: EMPTY":                         "password_env: the variable is not set",
		"ip: 192.168.1.50\n    password_file: pw.txt":                       "absolute path",
		"ip: 192.168.1.50\n    password_file: /nonexistent/pw":              "password_file: no such file",
		"ip: 192.168.1.50\n    password_env: PW\n    password_file: /x":     "only one of",
		"ip: 192.168.1.50\n    password_env: PW\n    defaults: {paper: a3}": "paper: want",
	} {
		_, err := parse(t, "scanners:\n  - id: s\n    type: scansnap\n    "+y+"\n", env)
		wantErr(t, err, want)
	}
	_, err := parse(t, `scanners:
  - id: a
    type: scansnap
    ip: 192.168.1.50
    password_env: PW
  - id: b
    type: scansnap
    ip: 192.168.1.50
    password_env: PW
`, env)
	wantErr(t, err, `already used by ScanSnap "a"`)
}

func TestSecretNeverInErrors(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(pw, []byte("s3cr3t-value-longer-than-sixteen"), 0o600)
	_, err := parse(t, "scanners:\n  - id: s\n    type: scansnap\n    ip: 192.168.1.50\n    password_file: "+pw+"\n", nil)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("got %v", err)
	}
	// A password pasted where a variable name or path belongs stays out too.
	for _, y := range []string{"password_env: s3cr3t", "password_file: s3cr3t", "password_file: /s3cr3t"} {
		_, err := parse(t, "scanners:\n  - id: s\n    type: scansnap\n    ip: 192.168.1.50\n    "+y+"\n", nil)
		if err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Fatalf("%s: got %v", y, err)
		}
	}
}

func TestESCLDeviceScans(t *testing.T) {
	f := escltest.Start()
	defer f.Close()
	devs, err := parse(t, "scanners:\n  - id: g\n    type: escl\n    url: "+f.URL+"\n    tls_insecure: true\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	g := devs[0]
	if st := g.Status(context.Background()); st.Label != "Ready" || st.Tone != scanner.ToneOK {
		t.Fatalf("status %+v", st)
	}
	s, err := g.ParseForm(form{"color": "gray", "dpi": "200", "region": "letter"}.get)
	if err != nil {
		t.Fatal(err)
	}
	var pages int
	if err := g.Scan(context.Background(), s, func(p scanner.Page) error { pages++; return nil }); err != nil || pages != 1 {
		t.Fatalf("scan: %v pages=%d", err, pages)
	}
	if ls := f.LastSettings(); !strings.Contains(ls, "Grayscale8") || !strings.Contains(ls, "<scan:XResolution>200<") {
		t.Fatalf("settings sent: %s", ls)
	}
	if _, err := g.ParseForm(form{"color": "pink"}.get); err == nil {
		t.Fatal("bad form accepted")
	}
}

type form map[string]string

func (f form) get(k string) string { return f[k] }

func TestExampleFileIsValid(t *testing.T) {
	d, _ := drivers(t, map[string]string{"SCANHUB_SCANSNAP_PASSWORD": "1234"})
	devs, err := scannerconfig.Load("../../scanners.example.yaml", d)
	if err != nil || len(devs) != 2 {
		t.Fatalf("%d devices, %v", len(devs), err)
	}
}

func TestOnlyOneYAMLDocument(t *testing.T) {
	for _, y := range []string{
		"---\n---\nscanners:\n  - id: g\n    type: escl\n    url: http://192.168.1.60\n",
		"scanners: []\n---\nfoo: 1\n",
	} {
		_, err := parse(t, y, nil)
		wantErr(t, err, "single YAML document")
	}
}

func TestESCLDefaultsUseFormValues(t *testing.T) {
	devs, err := parse(t, "scanners:\n  - id: g\n    type: escl\n    url: http://192.168.1.60\n    defaults: {color: Grey, region: MAX}\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range devs[0].Form() {
		want := map[string]string{"color": "gray", "region": "max", "dpi": "300"}[f.Name]
		found := false
		for _, o := range f.Options {
			found = found || o.Value == f.Value
		}
		if f.Value != want || !found {
			t.Fatalf("field %s: value %q (want %q), matches an option: %v", f.Name, f.Value, want, found)
		}
	}
}
