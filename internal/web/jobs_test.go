package web

import (
	"context"
	"errors"
	"testing"
)

func TestSessionLockExcludesScansAndSends(t *testing.T) {
	j := newJobs(context.Background())
	release := make(chan struct{})
	if _, err := j.start("glass", "flatbed", "s1", func(context.Context, func()) error { <-release; return nil }); err != nil {
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
	if _, err := j.start("feeder", "feeder", "s1", func(context.Context, func()) error { return nil }); !errors.Is(err, errSessionBusy) {
		t.Fatalf("scan during send: %v", err)
	}
	if _, err := j.start("feeder", "feeder", "s2", func(context.Context, func()) error { return nil }); err != nil {
		t.Fatalf("other session blocked: %v", err)
	}
	unlock()
	if _, err := j.start("glass", "flatbed", "s1", func(context.Context, func()) error { return nil }); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	j.wait()
}

func TestOneScanPerScannerAndPerSession(t *testing.T) {
	j := newJobs(context.Background())
	release := make(chan struct{})
	block := func(context.Context, func()) error { <-release; return nil }
	if _, err := j.start("feeder", "feeder", "s1", block); err != nil {
		t.Fatal(err)
	}
	if _, err := j.start("glass", "flatbed", "s1", block); !errors.Is(err, errSessionBusy) {
		t.Fatalf("second scanner into the same session: %v", err)
	}
	if _, err := j.start("feeder", "feeder", "s2", block); !errors.Is(err, errScannerBusy) {
		t.Fatalf("same scanner into another session: %v", err)
	}
	if _, err := j.start("glass", "flatbed", "s2", block); err != nil {
		t.Fatalf("other scanner, other session: %v", err)
	}
	if job, ok := j.runningFor("s2"); !ok || job.Scanner != "glass" {
		t.Fatalf("runningFor s2: %+v %v", job, ok)
	}
	close(release)
	j.wait()
	if _, ok := j.runningFor("s1"); ok {
		t.Fatal("finished job still running")
	}
	if _, err := j.start("glass", "flatbed", "s1", func(context.Context, func()) error { return nil }); err != nil {
		t.Fatalf("after the scans: %v", err)
	}
	j.wait()
}
