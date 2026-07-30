package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// MaxJobs caps how many completed jobs are remembered, so a long-lived daemon
// doesn't accumulate them without bound.
const MaxJobs = 50

// JobStore holds running and recently finished jobs.
//
// OnFinish, when set, is called once per job as it completes. It is the single
// place a finished deploy becomes observable to anything outside the store.
type JobStore struct {
	mu      sync.RWMutex
	jobs    map[string]*proto.Job
	order   []string
	waiters map[string][]chan struct{}
	seq     int

	// OnFinish is invoked with a copy of each job as it completes.
	OnFinish func(proto.Job)
}

func NewJobStore() *JobStore {
	return &JobStore{
		jobs:    map[string]*proto.Job{},
		waiters: map[string][]chan struct{}{},
	}
}

// Create registers a new job and returns it.
func (s *JobStore) Create(kind, service, release, from string) *proto.Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	job := &proto.Job{
		ID:        fmt.Sprintf("j%d-%s", s.seq, service),
		Kind:      kind,
		Service:   service,
		Release:   release,
		From:      from,
		State:     proto.JobPending,
		Phase:     proto.PhaseQueued,
		StartedAt: time.Now().UTC(),
	}

	s.jobs[job.ID] = job
	s.order = append(s.order, job.ID)
	s.trimLocked()

	// Return a copy so callers cannot mutate store state by accident.
	out := *job
	return &out
}

// trimLocked drops the oldest finished jobs beyond MaxJobs. Running jobs are
// never trimmed — losing the record of work still in flight would be worse than
// using a little more memory.
func (s *JobStore) trimLocked() {
	for len(s.order) > MaxJobs {
		for i, id := range s.order {
			if j, ok := s.jobs[id]; ok && j.State.Done() {
				delete(s.jobs, id)
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
			if i == len(s.order)-1 {
				return // nothing finished to reclaim
			}
		}
	}
}

// Get returns a snapshot of a job.
func (s *JobStore) Get(id string) (*proto.Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	j, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	out := *j
	out.Events = append([]proto.JobEvent(nil), j.Events...)
	return &out, true
}

// List returns snapshots of all known jobs, newest first.
func (s *JobStore) List() []*proto.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*proto.Job, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		if j, ok := s.jobs[s.order[i]]; ok {
			c := *j
			c.Events = append([]proto.JobEvent(nil), j.Events...)
			out = append(out, &c)
		}
	}
	return out
}

// Event appends a progress line and wakes anyone waiting on the job.
func (s *JobStore) Event(id, phase, format string, args ...any) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if ok {
		j.Phase = phase
		j.Events = append(j.Events, proto.JobEvent{
			At:      time.Now().UTC(),
			Phase:   phase,
			Message: fmt.Sprintf(format, args...),
		})
	}
	s.notifyLocked(id)
	s.mu.Unlock()
}

// Start marks a job as running.
func (s *JobStore) Start(id string) {
	s.mu.Lock()
	if j, ok := s.jobs[id]; ok {
		j.State = proto.JobRunning
	}
	s.notifyLocked(id)
	s.mu.Unlock()
}

// Finish records a job's outcome.
//
// OnFinish is called here rather than at the seventeen places that call this,
// which is the whole reason it is a hook: a new deploy path added later notifies
// without anyone remembering to wire it up. Getting that wrong would mean a
// deploy nobody was told about, which is the one outcome this must not have.
func (s *JobStore) Finish(id string, err error, rolledBack bool) {
	s.mu.Lock()
	var done proto.Job
	if j, ok := s.jobs[id]; ok {
		j.FinishedAt = time.Now().UTC()
		j.Phase = proto.PhaseDone
		j.RolledBack = rolledBack
		if err != nil {
			j.State = proto.JobFailed
			j.Error = err.Error()
		} else {
			j.State = proto.JobSucceeded
		}
		done = *j
	}
	s.notifyLocked(id)
	hook := s.OnFinish
	s.mu.Unlock()

	// Outside the lock: delivery can touch the network, and holding the store
	// while it does would stall every other job on the host.
	if hook != nil && done.ID != "" {
		hook(done)
	}
}

// notifyLocked wakes every waiter for a job. Callers must hold the lock.
func (s *JobStore) notifyLocked(id string) {
	for _, ch := range s.waiters[id] {
		close(ch)
	}
	delete(s.waiters, id)
}

// Wait blocks until the job changes or ctx is done.
//
// This is what lets the CLI stream progress without polling — and, crucially,
// what lets it stop watching at any moment without affecting the job.
func (s *JobStore) Wait(ctx context.Context, id string, afterEvents int) (*proto.Job, bool) {
	for {
		s.mu.Lock()
		j, ok := s.jobs[id]
		if !ok {
			s.mu.Unlock()
			return nil, false
		}
		if len(j.Events) > afterEvents || j.State.Done() {
			out := *j
			out.Events = append([]proto.JobEvent(nil), j.Events...)
			s.mu.Unlock()
			return &out, true
		}

		ch := make(chan struct{})
		s.waiters[id] = append(s.waiters[id], ch)
		s.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			out, _ := s.Get(id)
			return out, out != nil
		}
	}
}

// Active reports whether a service already has a job in flight, so a second
// deploy cannot race the first.
func (s *JobStore) Active(service string) (*proto.Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, id := range s.order {
		j, ok := s.jobs[id]
		if ok && j.Service == service && !j.State.Done() {
			out := *j
			return &out, true
		}
	}
	return nil, false
}
