package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var (
	// errScannerBusy means a job is already running on that scanner.
	errScannerBusy = errors.New("a scan is already running on this scanner")
	// errSessionBusy means the session is being scanned into, sent or deleted.
	errSessionBusy = errors.New("this session is busy")
)

// JobState of a scan job.
type JobState string

const (
	JobRunning JobState = "running"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
)

// Job is a snapshot of an asynchronous scan.
type Job struct {
	ID        string
	SessionID string
	Scanner   string
	State     JobState
	Pages     int
	Err       string
	Started   time.Time
	Finished  time.Time
}

// jobs runs at most one scan per scanner. Scans outlive the HTTP request
// (htmx times out after 60 s); the page polls the job instead.
type jobs struct {
	ctx     context.Context
	mu      sync.Mutex
	byID    map[string]*Job
	running map[string]string // scanner -> job ID
	locked  map[string]bool   // sessions being sent or deleted
	wg      sync.WaitGroup
}

func newJobs(ctx context.Context) *jobs {
	return &jobs{ctx: ctx, byID: map[string]*Job{}, running: map[string]string{}, locked: map[string]bool{}}
}

// lockSession reserves a session for a send/delete: it fails while a scan
// runs into it or another send/delete holds it. Call the returned unlock.
func (j *jobs) lockSession(sessionID string) (func(), error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.locked[sessionID] {
		return nil, errSessionBusy
	}
	for _, id := range j.running {
		if j.byID[id].SessionID == sessionID {
			return nil, errSessionBusy
		}
	}
	j.locked[sessionID] = true
	return func() {
		j.mu.Lock()
		delete(j.locked, sessionID)
		j.mu.Unlock()
	}, nil
}

// start launches fn; it calls addPage() after each stored page.
func (j *jobs) start(scanner, sessionID string, fn func(ctx context.Context, addPage func()) error) (Job, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, busy := j.running[scanner]; busy {
		return Job{}, errScannerBusy
	}
	if j.locked[sessionID] {
		return Job{}, errSessionBusy
	}
	j.pruneLocked()
	var b [8]byte
	rand.Read(b[:])
	job := &Job{ID: hex.EncodeToString(b[:]), SessionID: sessionID, Scanner: scanner, State: JobRunning, Started: time.Now()}
	j.byID[job.ID] = job
	j.running[scanner] = job.ID
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		err := fn(j.ctx, func() {
			j.mu.Lock()
			job.Pages++
			j.mu.Unlock()
		})
		j.mu.Lock()
		defer j.mu.Unlock()
		job.Finished = time.Now()
		job.State = JobDone
		if err != nil {
			job.State, job.Err = JobFailed, err.Error()
		}
		delete(j.running, scanner)
	}()
	return *job, nil
}

func (j *jobs) get(id string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.byID[id]
	if !ok {
		return Job{}, false
	}
	return *job, true
}

// runningFor returns the running job of a session, if any.
func (j *jobs) runningFor(sessionID string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, id := range j.running {
		if job := j.byID[id]; job.SessionID == sessionID {
			return *job, true
		}
	}
	return Job{}, false
}

func (j *jobs) pruneLocked() {
	for id, job := range j.byID {
		if job.State != JobRunning && time.Since(job.Finished) > time.Hour {
			delete(j.byID, id)
		}
	}
}

// wait blocks until running jobs have returned (after ctx is cancelled).
func (j *jobs) wait() { j.wg.Wait() }
