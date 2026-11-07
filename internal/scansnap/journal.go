package scansnap

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/guigui42/scan-hub/internal/fsutil"
)

// ErrDirtyJournal is returned when a claim would overwrite the token of a
// session that was never released.
var ErrDirtyJournal = errors.New("journal is dirty with another token")

// journalState is persisted before every RESERVE so that, after a crash,
// scan-hub can prove which stale session is its own.
type journalState struct {
	Token     string    `json:"token"`
	Dirty     bool      `json:"dirty"`
	ScannerIP string    `json:"scanner_ip,omitempty"` // empty in journals written before multi-scanner support
	Released  bool      `json:"released,omitempty"`   // released on request: don't claim until taken back
	Updated   time.Time `json:"updated"`
}

// belongsTo reports whether a dirty token may be reused for the scanner at ip.
func (st journalState) belongsTo(ip string) bool {
	return st.ScannerIP == "" || st.ScannerIP == ip
}

type journal struct {
	path string
	ip   string // scanner the tokens are for
}

func (j journal) load() (journalState, error) {
	var st journalState
	b, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("journal %s: %w", j.path, err)
	}
	return st, nil
}

func (st journalState) token() ([8]byte, bool) {
	var t [8]byte
	b, err := hex.DecodeString(st.Token)
	if err != nil || len(b) != 8 {
		return t, false
	}
	copy(t[:], b)
	return t, true
}

// begin marks token as possibly held. It refuses to replace a dirty journal
// that records a different token.
func (j journal) begin(token [8]byte) error {
	st, err := j.load()
	if err != nil {
		return err
	}
	h := hex.EncodeToString(token[:])
	if st.Dirty && st.Token != h {
		return ErrDirtyJournal
	}
	return j.write(journalState{Token: h, Dirty: true, ScannerIP: j.ip, Updated: time.Now().UTC()})
}

// clean records that no session of ours is held.
func (j journal) clean() error {
	st, err := j.load()
	if err != nil {
		return err
	}
	st.Dirty = false
	st.ScannerIP = j.ip
	st.Updated = time.Now().UTC()
	return j.write(st)
}

// setReleased records whether the scanner is released on request, keeping
// any token state.
func (j journal) setReleased(released bool) error {
	st, err := j.load()
	if err != nil {
		return err
	}
	st.Released = released
	st.Updated = time.Now().UTC()
	return j.write(st)
}

func (j journal) write(st journalState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(j.path, b, 0o600)
}

// JournalPath is where the ownership journal of the ScanSnap with id lives
// under dir (the data directory's scansnap folder).
func JournalPath(dir, id string) string { return filepath.Join(dir, id, "journal.json") }

// PrepareJournals checks the journals under dir before any manager starts,
// given the configured ScanSnaps (id -> IPv4). A journal left dirty by a
// crash holds the only proof of a session on one physical scanner, so it is
// never reused for another one:
//   - the legacy single-scanner dir/journal.json moves to the only configured
//     ScanSnap; with several, a dirty or unreadable one stops startup;
//   - a dirty journal recorded for another IP stops startup;
//   - a dirty journal of an id that is no longer configured stops startup.
func PrepareJournals(dir string, scanners map[string]string) error {
	var errs []error
	legacy := filepath.Join(dir, "journal.json")
	if _, err := os.Stat(legacy); err == nil {
		st, lerr := journal{path: legacy}.load()
		switch {
		case len(scanners) == 1:
			for id := range scanners {
				target := JournalPath(dir, id)
				if _, err := os.Stat(target); err == nil {
					errs = append(errs, fmt.Errorf("both %s and %s exist: remove the stale one", legacy, target))
					break
				}
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					errs = append(errs, err)
					break
				}
				if err := os.Rename(legacy, target); err != nil {
					errs = append(errs, err)
				}
			}
		case len(scanners) > 1 && lerr != nil:
			errs = append(errs, fmt.Errorf("%w (with several ScanSnaps it cannot be assigned: fix or delete it)", lerr))
		case len(scanners) > 1 && st.Dirty:
			errs = append(errs, fmt.Errorf("%s records an unreleased session but several ScanSnaps are configured: start once with only the ScanSnap it belongs to", legacy))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := JournalPath(dir, e.Name())
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		st, err := journal{path: path}.load()
		ip, configured := scanners[e.Name()]
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%w (fix or delete it)", err))
		case !st.Dirty:
		case !configured:
			errs = append(errs, fmt.Errorf("%s records an unreleased session of ScanSnap %q, which is no longer configured: restore it (same id and IP) for one start, or delete the file if that scanner was reset", path, e.Name()))
		case !st.belongsTo(ip):
			errs = append(errs, fmt.Errorf("%s records an unreleased session of the ScanSnap at %s, but %q is now configured at %s: restore the IP for one start, or delete the file if that scanner was reset", path, st.ScannerIP, e.Name(), ip))
		}
	}
	return errors.Join(errs...)
}
