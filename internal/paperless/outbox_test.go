package paperless_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guigui42/scan-hub/internal/paperless"
	"github.com/guigui42/scan-hub/internal/paperless/paperlesstest"
)

const token = "s3cr3t-token-value"

type env struct {
	fake *paperlesstest.Server
	ob   *paperless.Outbox
	dir  string
	logs *bytes.Buffer
}

func setup(t *testing.T, opt paperless.Options, prep func(dir string)) *env {
	t.Helper()
	fake := paperlesstest.Start(token)
	t.Cleanup(fake.Close)
	return start(t, fake, token, t.TempDir(), opt, prep)
}

func start(t *testing.T, fake *paperlesstest.Server, tok, dir string, opt paperless.Options, prep func(dir string)) *env {
	t.Helper()
	if prep != nil {
		prep(dir)
	}
	c, err := paperless.NewClient(fake.URL, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	opt.Dir = dir
	opt.Logger = slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	opt.MinFreeBytes = -1
	opt.PollMin, opt.PollMax = 10*time.Millisecond, 50*time.Millisecond
	opt.RetryMin, opt.RetryMax = 10*time.Millisecond, 50*time.Millisecond
	if opt.UncertainGrace == 0 {
		opt.UncertainGrace = 200 * time.Millisecond
	}
	ob, err := paperless.Open(c, opt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ob.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; ob.Close() })
	return &env{fake: fake, ob: ob, dir: dir, logs: logs}
}

type lockedWriter struct {
	mu chan struct{}
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	if l.mu == nil {
		l.mu = make(chan struct{}, 1)
	}
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	return l.w.Write(p)
}

var pdfBytes = []byte("%PDF-1.3 fake document\n%%EOF\n")

func enqueue(t *testing.T, ob *paperless.Outbox, title string) paperless.Item {
	t.Helper()
	it, err := ob.Enqueue(paperless.EnqueueMeta{Title: title, Source: "test", Pages: 1}, func(w io.Writer) error {
		_, err := w.Write(pdfBytes)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func wait(t *testing.T, ob *paperless.Outbox, id string, want paperless.State) paperless.Item {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var it paperless.Item
	for time.Now().Before(deadline) {
		it, _ = ob.Get(id)
		if it.State == want {
			return it
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("item %s: state %q (detail %q), want %q", id, it.State, it.Detail, want)
	return it
}

func pdfExists(dir, id string) bool {
	_, err := os.Stat(filepath.Join(dir, id+".pdf"))
	return err == nil
}

func TestSuccessDeletesPDF(t *testing.T) {
	e := setup(t, paperless.Options{Tags: []int{7, 9}}, nil)
	e.fake.Set(func(s *paperlesstest.Server) { s.PendingFor = 2 })
	it := enqueue(t, e.ob, "Facture EDF")
	got := wait(t, e.ob, it.ID, paperless.StateSuccess)
	if got.DocumentID == 0 {
		t.Fatal("no document id")
	}
	if pdfExists(e.dir, it.ID) {
		t.Fatal("PDF kept after success")
	}
	ups := e.fake.Uploads()
	if len(ups) != 1 || ups[0].Filename != it.Filename() || ups[0].Title != "Facture EDF" ||
		strings.Join(ups[0].Tags, ",") != "7,9" || !bytes.Equal(ups[0].Body, pdfBytes) {
		t.Fatalf("uploads %+v", ups)
	}
	if !strings.Contains(it.Filename(), it.ID) {
		t.Fatal("filename must contain the outbox ID")
	}
}

func TestDuplicateIsTerminalSuccess(t *testing.T) {
	e := setup(t, paperless.Options{}, nil)
	e.fake.Set(func(s *paperlesstest.Server) { s.Next = paperlesstest.Duplicate })
	it := enqueue(t, e.ob, "")
	got := wait(t, e.ob, it.ID, paperless.StateDuplicate)
	if got.DuplicateOf != 42 || pdfExists(e.dir, it.ID) {
		t.Fatalf("%+v pdf=%v", got, pdfExists(e.dir, it.ID))
	}
}

func TestFailureKeepsPDFThenRetry(t *testing.T) {
	e := setup(t, paperless.Options{}, nil)
	e.fake.Set(func(s *paperlesstest.Server) { s.Next = paperlesstest.Fail })
	it := enqueue(t, e.ob, "")
	got := wait(t, e.ob, it.ID, paperless.StateFailure)
	if got.Detail != "PDF is broken" || !pdfExists(e.dir, it.ID) {
		t.Fatalf("%+v pdf=%v", got, pdfExists(e.dir, it.ID))
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(e.fake.Uploads()); n != 1 {
		t.Fatalf("failure was retried automatically: %d uploads", n)
	}
	e.fake.Set(func(s *paperlesstest.Server) { s.Next = paperlesstest.Succeed })
	if _, err := e.ob.Retry(it.ID); err != nil {
		t.Fatal(err)
	}
	wait(t, e.ob, it.ID, paperless.StateSuccess)
	ups := e.fake.Uploads()
	if len(ups) != 2 || ups[0].Filename == ups[1].Filename {
		t.Fatalf("retry must use a new filename: %+v", ups)
	}
}

func TestServerBusyRetries(t *testing.T) {
	e := setup(t, paperless.Options{}, nil)
	e.fake.Set(func(s *paperlesstest.Server) { s.FailUploads = 2 })
	it := enqueue(t, e.ob, "")
	wait(t, e.ob, it.ID, paperless.StateSuccess)
	if n := len(e.fake.Uploads()); n != 1 {
		t.Fatalf("uploads %d", n)
	}
}

func TestLostResponseResolvedByFilename(t *testing.T) {
	e := setup(t, paperless.Options{}, nil)
	e.fake.Set(func(s *paperlesstest.Server) { s.DropAfter = 1 })
	it := enqueue(t, e.ob, "")
	wait(t, e.ob, it.ID, paperless.StateSuccess)
	if n := len(e.fake.Uploads()); n != 1 {
		t.Fatalf("uncertain upload was re-sent: %d uploads", n)
	}
}

func TestCrashDuringPostThenLost(t *testing.T) {
	fake := paperlesstest.Start(token)
	defer fake.Close()
	id := "0123456789abcdef"
	e := start(t, fake, token, t.TempDir(), paperless.Options{UncertainGrace: 100 * time.Millisecond}, func(dir string) {
		now := time.Now().UTC()
		it := paperless.Item{ID: id, Source: "test", Size: int64(len(pdfBytes)), Created: now, Updated: now,
			State: paperless.StatePosting, Attempt: 1, Since: now}
		b, _ := json.Marshal(it)
		os.WriteFile(filepath.Join(dir, id+".json"), b, 0o600)
		os.WriteFile(filepath.Join(dir, id+".pdf"), pdfBytes, 0o600)
	})
	got := wait(t, e.ob, id, paperless.StateSuccess)
	ups := fake.Uploads()
	if len(ups) != 1 || got.Attempt != 2 || ups[0].Filename != got.Filename() {
		t.Fatalf("attempt %d uploads %+v", got.Attempt, ups)
	}
}

func TestOrphanPDFAdopted(t *testing.T) {
	fake := paperlesstest.Start(token)
	defer fake.Close()
	id := "fedcba9876543210"
	e := start(t, fake, token, t.TempDir(), paperless.Options{}, func(dir string) {
		os.WriteFile(filepath.Join(dir, id+".pdf"), pdfBytes, 0o600)
	})
	wait(t, e.ob, id, paperless.StateSuccess)
}

func TestFullAndDiscard(t *testing.T) {
	e := setup(t, paperless.Options{MaxBytes: int64(len(pdfBytes)) + 5}, nil)
	e.fake.Set(func(s *paperlesstest.Server) { s.Next = paperlesstest.Fail })
	it := enqueue(t, e.ob, "")
	wait(t, e.ob, it.ID, paperless.StateFailure)
	_, err := e.ob.Enqueue(paperless.EnqueueMeta{}, func(w io.Writer) error { _, err := w.Write(pdfBytes); return err })
	if !errors.Is(err, paperless.ErrFull) {
		t.Fatalf("want ErrFull, got %v", err)
	}
	ents, _ := filepath.Glob(filepath.Join(e.dir, "[0-9a-f]*"))
	if len(ents) != 2 {
		t.Fatalf("leftover files after refused enqueue: %v", ents)
	}
	if tmp, _ := filepath.Glob(filepath.Join(e.dir, ".enqueue-*")); len(tmp) != 0 {
		t.Fatalf("temp files left: %v", tmp)
	}
	if err := e.ob.Discard(it.ID); err != nil {
		t.Fatal(err)
	}
	if pdfExists(e.dir, it.ID) {
		t.Fatal("discarded PDF still present")
	}
	ok := enqueue(t, e.ob, "")
	e.fake.Set(func(s *paperlesstest.Server) { s.Next = paperlesstest.Succeed })
	if err := e.ob.Discard(ok.ID); !errors.Is(err, paperless.ErrState) {
		t.Fatalf("discard of a non-failed item: %v", err)
	}
}

func TestStoppedCountsAsDelivered(t *testing.T) {
	e := setup(t, paperless.Options{}, nil)
	e.fake.Set(func(s *paperlesstest.Server) { s.Next = paperlesstest.Stopped })
	it := enqueue(t, e.ob, "")
	got := wait(t, e.ob, it.ID, paperless.StateSuccess)
	if !strings.Contains(got.Detail, "Barcode") {
		t.Fatalf("detail %q", got.Detail)
	}
}

func TestBadTokenRetriesAndNeverLogsIt(t *testing.T) {
	fake := paperlesstest.Start(token)
	defer fake.Close()
	e := start(t, fake, "wrong-token-zzz", t.TempDir(), paperless.Options{}, nil)
	it := enqueue(t, e.ob, "")
	time.Sleep(100 * time.Millisecond)
	got, _ := e.ob.Get(it.ID)
	if got.State != paperless.StatePending || !strings.Contains(got.Detail, "401") || !pdfExists(e.dir, it.ID) {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(e.logs.String(), "wrong-token-zzz") {
		t.Fatal("token logged")
	}
}

func TestTaskListShapes(t *testing.T) {
	fake := paperlesstest.Start(token)
	defer fake.Close()
	c, _ := paperless.NewClient(fake.URL, token, nil)
	ts, err := c.TasksByFilename(context.Background(), "nothing-matches")
	if err != nil || len(ts) != 0 {
		t.Fatalf("empty paginated list: %v %v", ts, err)
	}
	if _, found, err := c.Task(context.Background(), "nope"); err != nil || found {
		t.Fatalf("unknown task: found=%v err=%v", found, err)
	}
}

func TestSecondOpenIsRefused(t *testing.T) {
	e := setup(t, paperless.Options{}, nil)
	c, _ := paperless.NewClient(e.fake.URL, token, nil)
	if _, err := paperless.Open(c, paperless.Options{Dir: e.dir}); !errors.Is(err, paperless.ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
}
