package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// job tracks one background operation (an install or a bootstrap) so the
// console can poll progress instead of holding a request open for a Helm run.
type job struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	Name     string     `json:"name"`
	Status   string     `json:"status"` // running | done | error
	Log      []string   `json:"log"`
	Result   any        `json:"result,omitempty"`
	Error    string     `json:"error,omitempty"`
	Warning  string     `json:"warning,omitempty"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
}

type jobStore struct {
	mu       sync.Mutex
	m        map[string]*job
	order    []string
	installs atomic.Int64
	errors   atomic.Int64
}

func newJobStore() *jobStore { return &jobStore{m: map[string]*job{}} }

// start runs fn in the background. The progress callback appends to the job's
// log (bounded) so a long install or build can be followed from the console.
// maxFinishedJobs bounds the job history. Each finished job keeps up to 500 log
// lines; a console that runs for months would otherwise grow without limit.
const maxFinishedJobs = 100

func (s *jobStore) start(kind, name string, fn func(progress func(string)) (any, error)) job {
	j := &job{ID: randomID(), Kind: kind, Name: name, Status: "running", Started: time.Now(), Log: []string{}}
	s.mu.Lock()
	s.m[j.ID] = j
	s.order = append(s.order, j.ID)
	// Snapshot while the lock is held: a fast fn can finish (and mutate j)
	// before start returns.
	snap := j.snapshot()
	s.mu.Unlock()
	if kind == "install" {
		s.installs.Add(1)
	}

	progress := func(line string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(j.Log) < 500 {
			j.Log = append(j.Log, line)
		}
	}

	go func() {
		result, err := fn(progress)
		now := time.Now()
		s.mu.Lock()
		defer s.mu.Unlock()
		j.Finished = &now
		j.Result = result
		if err != nil {
			j.Status = "error"
			j.Error = err.Error()
			s.errors.Add(1)
		} else {
			j.Status = "done"
		}
		s.evictFinishedLocked()
	}()
	return snap
}

// evictFinishedLocked drops the oldest finished jobs once the history grows
// past maxFinishedJobs. Caller holds the lock.
func (s *jobStore) evictFinishedLocked() {
	finished := 0
	for _, id := range s.order {
		if s.m[id].Finished != nil {
			finished++
		}
	}
	if finished <= maxFinishedJobs {
		return
	}
	drop := finished - maxFinishedJobs
	kept := s.order[:0]
	for _, id := range s.order {
		if drop > 0 && s.m[id].Finished != nil {
			delete(s.m, id)
			drop--
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
}

func (s *jobStore) get(id string) (job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.m[id]
	if !ok {
		return job{}, false
	}
	return j.snapshot(), true
}

func (s *jobStore) list() []job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]job, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.m[id].snapshot())
	}
	return out
}

func (j *job) snapshot() job {
	c := *j
	c.Log = append([]string(nil), j.Log...)
	return c
}

func randomID() string {
	b := make([]byte, 8)
	// A failed rand.Read is catastrophic (predictable job/session ids); fail
	// closed rather than hand out a zero id.
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("generating a random id: %v", err)
	}
	return hex.EncodeToString(b)
}
