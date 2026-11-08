// Package scanner defines what the rest of scan-hub needs from a scanner,
// whatever its protocol. Drivers live in subpackages (escldrv, scansnapdrv);
// the scanner list is loaded by internal/scannerconfig.
package scanner

import (
	"context"
	"errors"
	"time"
)

// Kind tells the UI how pages are fed.
type Kind string

const (
	Feeder  Kind = "feeder"  // sheet feeder: a batch per scan
	Flatbed Kind = "flatbed" // glass: one page per pass
)

// Tone is the colour of a status stamp.
type Tone string

const (
	ToneOK   Tone = "ok"
	ToneBusy Tone = "busy"
	ToneBad  Tone = "bad"
	ToneIdle Tone = "idle"
)

// Fact is one line of a device card. Signal facts render as 0–3 bars.
type Fact struct {
	Label  string
	Value  string
	Signal bool
	Level  int
}

// Status is a snapshot for the UI. Drivers must return it quickly (cache any
// network probe).
type Status struct {
	Tone   Tone
	Label  string // short state, e.g. "Ready", "Offline"
	Detail string // optional explanation
	Facts  []Fact
	Raw    any // driver-specific details for /status (no secrets)
}

// FieldType is the input type of a form field.
type FieldType string

const (
	Select   FieldType = "select"
	Checkbox FieldType = "checkbox"
)

// Option is one choice of a select field.
type Option struct{ Value, Label string }

// Field is one scan setting in the UI form, with its default.
type Field struct {
	Name    string
	Label   string
	Type    FieldType
	Options []Option // Select
	Value   string   // Select: default value
	Checked bool     // Checkbox: default
}

// Settings are driver-specific scan settings from ParseForm or the button
// profile; only the driver that made them may use them.
type Settings any

// Page is one scanned image.
type Page struct {
	Data  []byte // JPEG or TIFF
	Sheet int    // physical sheet (0-based), 0 when unknown
	Side  int    // 0 front, 1 back
}

// Device is one configured scanner.
type Device interface {
	ID() string   // stable id from the scanner list ([a-z0-9-])
	Name() string // display name
	Kind() Kind
	Status(ctx context.Context) Status
	// Ready returns a user-facing error when a scan can't start now.
	Ready() error
	// Form lists the UI scan settings with their defaults.
	Form() []Field
	// ParseForm reads the form values (by Field.Name).
	ParseForm(get func(name string) string) (Settings, error)
	// Scan runs one scan, calling onPage for each page in order; an error
	// from onPage stops it. Pages delivered before an error remain valid.
	// Errors wrap the sentinels below when they apply.
	Scan(ctx context.Context, s Settings, onPage func(Page) error) error
}

// Runner is a Device with a background loop (e.g. holding a connection).
// Run returns when ctx ends; it runs once.
type Runner interface {
	Run(ctx context.Context) error
}

// ButtonEvent is one press of a scanner's own Scan button.
type ButtonEvent struct {
	At  time.Time
	Seq uint32
}

// Button is a Device with a physical Scan button. Buttons is closed when the
// device stops (Runner.Run returned).
type Button interface {
	Buttons() <-chan ButtonEvent
	ButtonSettings() Settings
}

// Releaser is a Device that holds its scanner exclusively and can let go of
// it so another computer can use it, until taken back.
type Releaser interface {
	Release(ctx context.Context) error
	TakeBack(ctx context.Context) error
	Released() bool
}

// Errors drivers wrap so callers can react without knowing the protocol.
var (
	ErrBusy      = errors.New("scanner busy")
	ErrNotReady  = errors.New("scanner not ready")
	ErrNoPaper   = errors.New("no paper in the feeder")
	ErrCoverOpen = errors.New("feeder cover open")
	ErrJam       = errors.New("paper jam")
	ErrMultiFeed = errors.New("several sheets fed at once")
)

// Find returns the device with id.
func Find(devs []Device, id string) (Device, bool) {
	for _, d := range devs {
		if d.ID() == id {
			return d, true
		}
	}
	return nil, false
}

// explained is a user-facing message classified by a sentinel.
type explained struct {
	kind error
	text string
}

func (e explained) Error() string { return e.text }
func (e explained) Unwrap() error { return e.kind }

// Explain returns an error whose message is text, for showing to users, and
// which matches kind with errors.Is.
func Explain(kind error, text string) error { return explained{kind: kind, text: text} }
