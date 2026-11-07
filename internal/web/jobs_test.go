package web

import (
	"context"
	"errors"
	"testing"
)

func TestSessionLockExcludesScansAndSends(t *testing.T) {
	j := newJobs(context.Background())
	release := make(chan struct{})
	if _, err := j.start("epson", "s1", func(context.Context, func()) error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := j.lockSession("s1"); !errors.Is(err, errSessionBusy) {
		t.Fatalf("send during scan: %v", err)
	}
	close(release)
	j.wait()

	unlock, err := j.lockSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.lockSession("s1"); !errors.Is(err, errSessionBusy) {
		t.Fatal("double send allowed")
	}
	if _, err := j.start("scansnap", "s1", func(context.Context, func()) error { return nil }); !errors.Is(err, errSessionBusy) {
		t.Fatalf("scan during send: %v", err)
	}
	if _, err := j.start("scansnap", "s2", func(context.Context, func()) error { return nil }); err != nil {
		t.Fatalf("other session blocked: %v", err)
	}
	unlock()
	if _, err := j.start("epson", "s1", func(context.Context, func()) error { return nil }); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	j.wait()
}
