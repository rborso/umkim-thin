package main

import (
	"crypto/rand"
	"encoding/hex"
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
func (s *jobStore) start(kind, name string, fn func(progress func(string)) (any, error)) job {
	j := &job{ID: randomID(), Kind: kind, Name: name, Status: "running", Started: time.Now(), Log: []string{}}
	s.mu.Lock()
	s.m[j.ID] = j
	s.order = append(s.order, j.ID)
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
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now()
		j.Finished = &now
		j.Result = result
		if err != nil {
			j.Status = "error"
			j.Error = err.Error()
			s.errors.Add(1)
			return
		}
		j.Status = "done"
	}()
	return j.snapshot()
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
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
