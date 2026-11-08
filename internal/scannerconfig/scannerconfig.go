// Package scannerconfig loads the scanner list from a YAML file. Each entry
// has an id, a type and a display name; the rest is decoded by the driver of
// that type, strictly (unknown keys are errors).
package scannerconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/guigui42/scan-hub/internal/scanner"
	"go.yaml.in/yaml/v3"
)

// Common are the keys every entry has. Driver entry structs embed it with
// `yaml:",inline"`.
type Common struct {
	ID   string `yaml:"id"`
	Type string `yaml:"type"`
	Name string `yaml:"name"`
}

// Driver builds the devices of one type.
type Driver interface {
	// Build decodes the whole entry with decode (strict) and returns the
	// device. It must not contact the scanner.
	Build(c Common, decode func(v any) error) (scanner.Device, error)
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

const maxFileBytes = 1 << 20

// Load reads the scanner list at path. All problems are reported together.
func Load(path string, drivers map[string]Driver) ([]scanner.Device, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("scanner list: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("scanner list: %w", err)
	}
	if len(b) > maxFileBytes {
		return nil, fmt.Errorf("scanner list %s: larger than 1 MiB", path)
	}
	return Parse(b, drivers)
}

// Parse is Load on the file's contents.
func Parse(b []byte, drivers map[string]Driver) ([]scanner.Device, error) {
	var doc struct {
		Scanners []yaml.Node `yaml:"scanners"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("scanner list: %w", err)
	} else if err == nil {
		var extra yaml.Node
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("scanner list: want a single YAML document (found a second one after ---)")
		}
	}
	var (
		devs []scanner.Device
		errs []error
		seen = map[string]bool{}
	)
	for i, node := range doc.Scanners {
		raw, err := yaml.Marshal(&node)
		if err != nil {
			errs = append(errs, fmt.Errorf("scanners[%d]: %w", i, err))
			continue
		}
		var c Common
		if err := yaml.Unmarshal(raw, &c); err != nil {
			errs = append(errs, fmt.Errorf("scanners[%d]: %w", i, err))
			continue
		}
		where := fmt.Sprintf("scanners[%d]", i)
		if c.ID != "" {
			where += " (" + c.ID + ")"
		}
		var bad []error
		switch {
		case !idRe.MatchString(c.ID):
			bad = append(bad, fmt.Errorf("id: want 1-32 of a-z, 0-9 and '-' (not starting with '-'), got %q", c.ID))
		case seen[c.ID]:
			bad = append(bad, fmt.Errorf("id %q is used twice", c.ID))
		}
		seen[c.ID] = true
		if c.Name == "" {
			c.Name = c.ID
		}
		if len([]rune(c.Name)) > 60 || strings.ContainsFunc(c.Name, unicode.IsControl) {
			bad = append(bad, errors.New("name: want at most 60 printable characters"))
		}
		drv, ok := drivers[c.Type]
		if !ok {
			bad = append(bad, fmt.Errorf("type: want one of %s, got %q", typeList(drivers), c.Type))
		}
		if len(bad) > 0 {
			errs = append(errs, fmt.Errorf("%s: %w", where, errors.Join(bad...)))
			continue
		}
		dev, err := drv.Build(c, func(v any) error {
			d := yaml.NewDecoder(bytes.NewReader(raw))
			d.KnownFields(true)
			return d.Decode(v)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
			continue
		}
		devs = append(devs, dev)
	}
	return devs, errors.Join(errs...)
}

func typeList(drivers map[string]Driver) string {
	var names []string
	for k := range drivers {
		names = append(names, k)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// Secret reads a secret from exactly one of a file (absolute path) or an
// environment variable; neither may be empty.
func Secret(lookup func(string) (string, bool), file, env string) (string, error) {
	// Errors never echo file or env: a misconfigured entry may hold the
	// password itself instead of a path or variable name.
	switch {
	case file != "" && env != "":
		return "", errors.New("set only one of password_file and password_env")
	case file != "":
		if !filepath.IsAbs(file) {
			return "", errors.New("password_file must be an absolute path")
		}
		b, err := os.ReadFile(file)
		if err != nil {
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return "", fmt.Errorf("password_file: %w", err)
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
		return "", errors.New("password_file is empty")
	case env != "":
		if v, ok := lookup(env); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
		return "", errors.New("password_env: the variable is not set or empty")
	}
	return "", errors.New("password_file or password_env is required")
}
