package paperless

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/guigui42/scan-hub/internal/fsutil"
)

// State of an outbox item.
type State string

const (
	StatePending   State = "pending"   // waiting to be uploaded
	StatePosting   State = "posting"   // upload in progress (persisted before POST)
	StateUncertain State = "uncertain" // upload outcome unknown; resolved by filename
	StateAccepted  State = "accepted"  // Paperless returned a task; polling it
	StateSuccess   State = "success"   // document created (PDF deleted)
	StateDuplicate State = "duplicate" // Paperless already had it (PDF deleted)
	StateFailure   State = "failure"   // needs attention; PDF kept
)

// Done reports whether Paperless has the document (the PDF is gone).
func (s State) Done() bool { return s == StateSuccess || s == StateDuplicate }

var (
	// ErrFull is returned when the outbox or disk limits would be exceeded.
	ErrFull = errors.New("outbox full")
	// ErrNotFound is returned for unknown items.
	ErrNotFound = errors.New("outbox item not found")
	// ErrState is returned when an action doesn't apply to the item's state.
	ErrState = errors.New("not allowed in this state")
)

var (
	idPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
	// defaultTitle is what Paperless calls an untitled upload: our file name.
	defaultTitle = regexp.MustCompile(`^scan-\d{8}-\d{6}-[0-9a-f]{16}-a\d+$`)
)

// Item is one PDF on its way to Paperless.
type Item struct {
	ID          string    `json:"id"`
	Title       string    `json:"title,omitempty"`
	Source      string    `json:"source"`
	Pages       int       `json:"pages"`
	Size        int64     `json:"size"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	State       State     `json:"state"`
	Attempt     int       `json:"attempt"`
	Retries     int       `json:"retries"`
	NextAttempt time.Time `json:"next_attempt"`
	Since       time.Time `json:"since"` // entered the current state
	TaskID      string    `json:"task_id,omitempty"`
	DocumentID  int       `json:"document_id,omitempty"`
	DuplicateOf int       `json:"duplicate_of,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	// PaperlessTitle is the document's current title in Paperless, which
	// triage may have changed since the upload.
	PaperlessTitle string `json:"paperless_title,omitempty"`
}

// DocID is the Paperless document that holds this item, if known.
func (it Item) DocID() int { return cmp.Or(it.DocumentID, it.DuplicateOf) }

// DisplayTitle is the title Paperless shows now, unless Paperless only
// derived it from the upload file name; then it is the title we sent.
func (it Item) DisplayTitle() string {
	if t := it.PaperlessTitle; t != "" && !defaultTitle.MatchString(t) {
		return t
	}
	return it.Title
}

// Filename is the upload name for the current attempt. It is unique per
// item and attempt, so an uncertain upload can be found among the tasks.
func (it Item) Filename() string {
	return fmt.Sprintf("scan-%s-%s-a%d.pdf", it.Created.Format("20060102-150405"), it.ID, it.Attempt)
}

// Options configure the outbox. Zero values take the defaults.
type Options struct {
	Dir            string
	Tags           []int         // Paperless tag IDs added to every upload
	MaxBytes       int64         // pending PDFs, default 2 GiB
	MinFreeBytes   int64         // default 512 MiB; negative disables
	PollMin        time.Duration // default 2 s
	PollMax        time.Duration // default 30 s
	RetryMin       time.Duration // default 10 s
	RetryMax       time.Duration // default 15 min
	UncertainGrace time.Duration // how long a lost upload may take to appear, default 60 s
	UploadTimeout  time.Duration // default 10 min
	HistoryMaxAge  time.Duration // keep finished items this long, default 30 days
	TitleRefresh   time.Duration // re-read titles of delivered items, default 5 min; negative disables
	Logger         *slog.Logger
}

func (o *Options) defaults() {
	d := func(v *time.Duration, def time.Duration) {
		if *v == 0 {
			*v = def
		}
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 2 << 30
	}
	if o.MinFreeBytes == 0 {
		o.MinFreeBytes = 512 << 20
	}
	d(&o.PollMin, 2*time.Second)
	d(&o.PollMax, 30*time.Second)
	d(&o.RetryMin, 10*time.Second)
	d(&o.RetryMax, 15*time.Minute)
	d(&o.UncertainGrace, time.Minute)
	d(&o.UploadTimeout, 10*time.Minute)
	d(&o.HistoryMaxAge, 30*24*time.Hour)
	d(&o.TitleRefresh, 5*time.Minute)
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Outbox persists PDFs under Dir as <id>.pdf + <id>.json and uploads them
// from a single worker (Run).
type Outbox struct {
	opt    Options
	client *Client
	log    *slog.Logger
	kick   chan struct{}
	free   func(string) (int64, error)

	lock *os.File

	// Owned by the Run goroutine.
	nextTitles      time.Time
	titlesForbidden bool

	mu    sync.Mutex
	items map[string]Item
}

// ErrLocked means another process has the outbox open.
var ErrLocked = errors.New("outbox is in use by another scan-hub process")

// Open loads the outbox. Items left in "posting" by a crash become
// "uncertain"; a PDF without metadata is adopted as a new pending item.
func Open(client *Client, opt Options) (*Outbox, error) {
	opt.defaults()
	if err := fsutil.MkdirAllDurable(opt.Dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(opt.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, ErrLocked
	}
	o := &Outbox{
		opt: opt, client: client, log: opt.Logger.With("component", "outbox"),
		kick: make(chan struct{}, 1), free: freeBytes, items: map[string]Item{}, lock: lock,
	}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	ents, err := os.ReadDir(opt.Dir)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	pdfs := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		id, ext, _ := strings.Cut(name, ".")
		if !idPattern.MatchString(id) {
			continue
		}
		switch ext {
		case "pdf":
			pdfs[id] = true
		case "json":
			b, err := os.ReadFile(filepath.Join(opt.Dir, name))
			if err != nil {
				return nil, err
			}
			var it Item
			if err := json.Unmarshal(b, &it); err != nil || it.ID != id {
				o.log.Error("unreadable outbox item kept aside", "file", name, "err", err)
				continue
			}
			if it.State == StatePosting {
				it.State, it.Since, it.NextAttempt = StateUncertain, now, now
				it.Detail = "interrupted during upload"
				if err := o.save(it); err != nil {
					return nil, err
				}
			}
			o.items[id] = it
		}
	}
	for id := range pdfs {
		if _, ok := o.items[id]; ok {
			continue
		}
		fi, err := os.Stat(o.pdfPath(id))
		if err != nil {
			continue
		}
		it := Item{ID: id, Source: "recovered", Size: fi.Size(), Created: fi.ModTime().UTC(), Updated: now,
			State: StatePending, Attempt: 1, Since: now, NextAttempt: now, Detail: "recovered PDF without metadata"}
		o.log.Warn("adopting PDF without metadata", "id", id)
		if err := o.save(it); err != nil {
			return nil, err
		}
		o.items[id] = it
	}
	ok = true
	return o, nil
}

// Close releases the directory lock (after Run has returned).
func (o *Outbox) Close() error { return o.lock.Close() }

// EnqueueMeta describes a new PDF.
type EnqueueMeta struct {
	Title  string
	Source string
	Pages  int
}

// Enqueue writes a PDF (via write) into the outbox and schedules its upload.
// Once it returns, the PDF is durable and the caller may drop its pages.
func (o *Outbox) Enqueue(meta EnqueueMeta, write func(io.Writer) error) (Item, error) {
	if o.opt.MinFreeBytes >= 0 {
		if free, err := o.free(o.opt.Dir); err == nil && free < o.opt.MinFreeBytes {
			return Item{}, fmt.Errorf("%w: less than %d bytes free on disk", ErrFull, o.opt.MinFreeBytes)
		}
	}
	id := newID()
	tmp, err := os.CreateTemp(o.opt.Dir, ".enqueue-*.pdf")
	if err != nil {
		return Item{}, err
	}
	defer os.Remove(tmp.Name())
	if err := write(tmp); err != nil {
		tmp.Close()
		return Item{}, err
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Item{}, err
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if used := o.pendingBytesLocked(); used+size > o.opt.MaxBytes {
		return Item{}, fmt.Errorf("%w: %d of %d bytes waiting", ErrFull, used, o.opt.MaxBytes)
	}
	if err := os.Rename(tmp.Name(), o.pdfPath(id)); err != nil {
		return Item{}, err
	}
	now := time.Now().UTC()
	it := Item{ID: id, Title: meta.Title, Source: meta.Source, Pages: meta.Pages, Size: size,
		Created: now, Updated: now, State: StatePending, Attempt: 1, Since: now, NextAttempt: now}
	if err := o.save(it); err != nil {
		os.Remove(o.pdfPath(id))
		return Item{}, err
	}
	o.items[id] = it
	o.log.Info("enqueued", "id", id, "pages", meta.Pages, "bytes", size, "source", meta.Source)
	o.wake()
	return it, nil
}

// List returns all items, newest first.
func (o *Outbox) List() []Item {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Item, 0, len(o.items))
	for _, it := range o.items {
		out = append(out, it)
	}
	slices.SortFunc(out, func(a, b Item) int { return b.Created.Compare(a.Created) })
	return out
}

// Get returns one item.
func (o *Outbox) Get(id string) (Item, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	it, ok := o.items[id]
	if !ok {
		return Item{}, ErrNotFound
	}
	return it, nil
}

// Retry re-queues a failed item with a fresh upload name.
func (o *Outbox) Retry(id string) (Item, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	it, ok := o.items[id]
	switch {
	case !ok:
		return Item{}, ErrNotFound
	case it.State != StateFailure:
		return it, ErrState
	}
	now := time.Now().UTC()
	it.State, it.Attempt, it.Retries, it.Since, it.NextAttempt, it.TaskID = StatePending, it.Attempt+1, 0, now, now, ""
	it.Detail = "retry requested"
	if err := o.saveLocked(it); err != nil {
		return it, err
	}
	o.wake()
	return it, nil
}

// Discard deletes a failed item and its PDF. It is the only way an unsent
// PDF leaves the outbox, and it is always a user action.
func (o *Outbox) Discard(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	it, ok := o.items[id]
	switch {
	case !ok:
		return ErrNotFound
	case it.State != StateFailure:
		return ErrState
	}
	o.log.Warn("discarding unsent PDF on user request", "id", id, "detail", it.Detail)
	if err := os.Remove(o.pdfPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(o.jsonPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(o.items, id)
	return fsutil.SyncDir(o.opt.Dir)
}

// Run uploads and tracks items until ctx is cancelled.
func (o *Outbox) Run(ctx context.Context) {
	for {
		now := time.Now().UTC()
		for _, it := range o.due(now) {
			if ctx.Err() != nil {
				return
			}
			o.step(ctx, it)
		}
		if o.opt.TitleRefresh > 0 && !now.Before(o.nextTitles) {
			o.refreshTitles(ctx)
		}
		o.prune(now)
		wait := o.untilNext(time.Now().UTC())
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-o.kick:
		case <-t.C:
		}
		t.Stop()
	}
}

func (o *Outbox) wake() {
	select {
	case o.kick <- struct{}{}:
	default:
	}
}

func (o *Outbox) due(now time.Time) []Item {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []Item
	for _, it := range o.items {
		switch it.State {
		case StatePending, StateUncertain, StateAccepted:
			if !it.NextAttempt.After(now) {
				out = append(out, it)
			}
		}
	}
	slices.SortFunc(out, func(a, b Item) int { return a.Created.Compare(b.Created) })
	return out
}

func (o *Outbox) untilNext(now time.Time) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	next := 30 * time.Second
	if o.opt.TitleRefresh > 0 {
		next = min(next, max(o.nextTitles.Sub(now), 10*time.Millisecond))
	}
	for _, it := range o.items {
		switch it.State {
		case StatePending, StateUncertain, StateAccepted:
			next = min(next, max(it.NextAttempt.Sub(now), 10*time.Millisecond))
		}
	}
	return next
}

func (o *Outbox) step(ctx context.Context, it Item) {
	switch it.State {
	case StatePending:
		o.post(ctx, it)
	case StateUncertain:
		o.resolve(ctx, it)
	case StateAccepted:
		o.poll(ctx, it)
	}
}

func (o *Outbox) post(ctx context.Context, it Item) {
	log := o.log.With("id", it.ID, "file", it.Filename())
	f, err := os.Open(o.pdfPath(it.ID))
	if err != nil {
		o.transition(it, StateFailure, "PDF missing from outbox: "+err.Error())
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		o.transition(it, StateFailure, "PDF unreadable in outbox: "+err.Error())
		return
	}

	it.State, it.Since = StatePosting, time.Now().UTC()
	if err := o.update(it); err != nil {
		log.Error("persist before upload", "err", err)
		return
	}
	uctx, cancel := context.WithTimeout(ctx, o.opt.UploadTimeout)
	taskID, maybe, err := o.client.Upload(uctx, it.Filename(), f, fi.Size(), UploadMeta{Title: it.Title, Tags: o.opt.Tags})
	cancel()
	now := time.Now().UTC()
	switch {
	case err == nil:
		it.State, it.TaskID, it.Retries, it.Since, it.NextAttempt, it.Detail = StateAccepted, taskID, 0, now, now.Add(o.opt.PollMin), ""
		log.Info("uploaded", "task", taskID)
	case errors.Is(err, ErrPermanent):
		it.State, it.Since, it.Detail = StateFailure, now, err.Error()
		log.Error("upload rejected", "err", err)
	case maybe:
		it.State, it.Since, it.NextAttempt, it.Detail = StateUncertain, now, now.Add(o.opt.PollMin), err.Error()
		log.Warn("upload outcome unknown; will look it up by filename", "err", err)
	default:
		it.State, it.Since, it.Detail = StatePending, now, err.Error()
		it.NextAttempt = now.Add(o.backoff(&it, o.opt.RetryMin, o.opt.RetryMax))
		log.Warn("upload failed; will retry", "err", err, "next", it.NextAttempt)
	}
	if err := o.update(it); err != nil {
		log.Error("persist after upload", "err", err)
	}
}

// resolve finds out whether an upload with an unknown outcome reached
// Paperless, by its unique filename, before anything is re-sent.
func (o *Outbox) resolve(ctx context.Context, it Item) {
	log := o.log.With("id", it.ID, "file", it.Filename())
	tasks, err := o.client.TasksByFilename(ctx, it.ID)
	now := time.Now().UTC()
	if err != nil {
		it.Detail = "looking up upload: " + err.Error()
		it.NextAttempt = now.Add(o.backoff(&it, o.opt.PollMin, o.opt.RetryMax))
		o.update(it)
		return
	}
	for _, t := range tasks {
		if t.InputData.Filename == it.Filename() {
			log.Info("uncertain upload found in Paperless", "task", t.TaskID)
			it.State, it.TaskID, it.Retries, it.Since, it.NextAttempt, it.Detail = StateAccepted, t.TaskID, 0, now, now, ""
			o.update(it)
			return
		}
	}
	if now.Sub(it.Since) < o.opt.UncertainGrace {
		it.NextAttempt = now.Add(o.opt.PollMin)
		o.update(it)
		return
	}
	log.Info("uncertain upload never reached Paperless; re-sending")
	it.State, it.Attempt, it.Since, it.NextAttempt, it.Detail = StatePending, it.Attempt+1, now, now, "previous upload was lost"
	o.update(it)
}

func (o *Outbox) poll(ctx context.Context, it Item) {
	log := o.log.With("id", it.ID, "task", it.TaskID)
	t, found, err := o.client.Task(ctx, it.TaskID)
	now := time.Now().UTC()
	if err != nil || !found {
		if err != nil {
			it.Detail = "checking task: " + err.Error()
		} else {
			it.Detail = "task not visible yet"
		}
		it.NextAttempt = now.Add(o.backoff(&it, o.opt.PollMin, o.opt.PollMax))
		o.update(it)
		return
	}
	rd := t.ResultData
	switch {
	// Paperless 3.3 marks duplicates as "failure"; check before the status.
	case rd.DuplicateOf > 0:
		it.DuplicateOf = rd.DuplicateOf
		o.finish(it, StateDuplicate, fmt.Sprintf("already in Paperless as document %d", rd.DuplicateOf))
	case t.Status == "success" || t.Status == "SUCCESS":
		it.DocumentID = rd.DocumentID
		detail := ""
		if rd.DocumentID == 0 && rd.Reason != "" {
			detail = "consumed: " + rd.Reason
		}
		o.finish(it, StateSuccess, detail)
	case t.Status == "failure" || t.Status == "FAILURE" || t.Status == "revoked" || t.Status == "REVOKED":
		msg := rd.ErrorMessage
		if msg == "" {
			msg = "Paperless task " + strings.ToLower(t.Status)
		}
		log.Error("consumption failed", "err", msg)
		o.transition(it, StateFailure, msg)
	default: // pending, started
		it.Detail = "Paperless is processing (" + strings.ToLower(t.Status) + ")"
		it.NextAttempt = now.Add(o.backoff(&it, o.opt.PollMin, o.opt.PollMax))
		o.update(it)
	}
}

// finish records a terminal success and only then deletes the PDF.
func (o *Outbox) finish(it Item, st State, detail string) {
	o.transition(it, st, detail)
	if err := os.Remove(o.pdfPath(it.ID)); err != nil && !os.IsNotExist(err) {
		o.log.Warn("delete sent PDF", "id", it.ID, "err", err)
	}
	fsutil.SyncDir(o.opt.Dir)
	o.nextTitles = time.Time{} // pick up the title Paperless gave it
	o.log.Info("delivered", "id", it.ID, "state", st, "document", it.DocumentID, "duplicate_of", it.DuplicateOf)
}

func (o *Outbox) transition(it Item, st State, detail string) {
	now := time.Now().UTC()
	it.State, it.Since, it.Detail, it.NextAttempt = st, now, detail, time.Time{}
	if err := o.update(it); err != nil {
		o.log.Error("persist outbox item", "id", it.ID, "err", err)
	}
}

// refreshTitles copies the current Paperless title of delivered items, so
// documents renamed during triage show their new name. It needs the
// view_document permission; without it, it backs off and stays quiet.
func (o *Outbox) refreshTitles(ctx context.Context) {
	now := time.Now().UTC()
	o.nextTitles = now.Add(o.opt.TitleRefresh)
	var ids []int
	o.mu.Lock()
	for _, it := range o.items {
		if id := it.DocID(); it.State.Done() && id > 0 && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	o.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	slices.Sort(ids)
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	titles, err := o.client.Titles(c, ids)
	cancel()
	switch {
	case errors.Is(err, ErrForbidden):
		if !o.titlesForbidden {
			o.log.Warn("cannot read document titles; grant view_document to the Paperless user to show titles changed in Paperless")
			o.titlesForbidden = true
		}
		o.nextTitles = now.Add(max(o.opt.TitleRefresh, time.Hour))
		return
	case err != nil:
		o.log.Debug("refresh titles", "err", err)
		return
	}
	if o.titlesForbidden {
		o.log.Info("document titles readable again")
		o.titlesForbidden = false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, it := range o.items {
		t, ok := titles[it.DocID()]
		if !it.State.Done() || !ok || t == it.PaperlessTitle {
			continue
		}
		it.PaperlessTitle = t
		// Not saveLocked: a rename must not extend the item's history.
		if err := o.save(it); err != nil {
			o.log.Error("persist title", "id", id, "err", err)
			continue
		}
		o.items[id] = it
	}
}

// backoff returns the next delay and counts the retry.
func (o *Outbox) backoff(it *Item, lo, hi time.Duration) time.Duration {
	d := lo << min(it.Retries, 16)
	it.Retries++
	return min(d, hi)
}

// prune drops metadata of delivered items older than HistoryMaxAge.
func (o *Outbox) prune(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, it := range o.items {
		if it.State.Done() && now.Sub(it.Updated) > o.opt.HistoryMaxAge {
			os.Remove(o.jsonPath(id))
			delete(o.items, id)
		}
	}
}

func (o *Outbox) pendingBytesLocked() int64 {
	var n int64
	for _, it := range o.items {
		if !it.State.Done() {
			n += it.Size
		}
	}
	return n
}

func (o *Outbox) update(it Item) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.saveLocked(it)
}

func (o *Outbox) saveLocked(it Item) error {
	it.Updated = time.Now().UTC()
	if err := o.save(it); err != nil {
		return err
	}
	o.items[it.ID] = it
	return nil
}

func (o *Outbox) save(it Item) error {
	b, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(o.jsonPath(it.ID), b, 0o600)
}

func (o *Outbox) pdfPath(id string) string  { return filepath.Join(o.opt.Dir, id+".pdf") }
func (o *Outbox) jsonPath(id string) string { return filepath.Join(o.opt.Dir, id+".json") }

// PDFPath returns the on-disk PDF of an undelivered item (for preview).
func (o *Outbox) PDFPath(id string) (string, error) {
	it, err := o.Get(id)
	if err != nil {
		return "", err
	}
	if it.State.Done() {
		return "", ErrNotFound
	}
	return o.pdfPath(id), nil
}

func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
