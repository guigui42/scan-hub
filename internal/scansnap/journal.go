package scansnap

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/guigui42/scan-hub/internal/fsutil"
)

// ErrDirtyJournal is returned when a claim would overwrite the token of a
// session that was never released.
var ErrDirtyJournal = errors.New("journal is dirty with another token")

// journalState is persisted before every RESERVE so that, after a crash,
// scan-hub can prove which stale session is its own.
type journalState struct {
	Token   string    `json:"token"`
	Dirty   bool      `json:"dirty"`
	Updated time.Time `json:"updated"`
}

type journal struct{ path string }

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
	return j.write(journalState{Token: h, Dirty: true, Updated: time.Now().UTC()})
}

// clean records that no session of ours is held.
func (j journal) clean() error {
	st, err := j.load()
	if err != nil {
		return err
	}
	st.Dirty = false
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
