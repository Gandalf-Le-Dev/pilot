package agent

import (
	"context"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

// Health sampling for the public status page.
//
// This is the first caller of Probe outside a deploy. State observation says
// whether the process is up; only the probe says whether it answers, and a
// status page that showed a container returning 500s as "up" would be
// reporting the wrong thing to exactly the people who cannot check.
//
// History lives in memory, 24 hours per service, and the whole of it travels
// in every report. That is what lets the server keep no state on disk: after
// either side restarts, the next report carries everything the agent still
// has.

// HealthInterval is how often listed services are sampled.
const HealthInterval = statuspage.ReportInterval

// healthRing holds one service's buckets, indexed by time rather than by
// arrival. A slot whose start does not match the bucket being asked for is
// stale, so a gap — the agent was down for an hour — reads as no data
// instead of shifting every later bucket out of place.
type healthRing struct {
	current statuspage.State
	slots   [statuspage.HistoryBuckets]healthSlot
}

type healthSlot struct {
	start int64
	state statuspage.State
}

func slotIndex(start int64) int {
	width := int64(statuspage.BucketWidth / time.Second)
	return int((start / width) % statuspage.HistoryBuckets)
}

func (r *healthRing) record(at time.Time, s statuspage.State) {
	start := statuspage.BucketStart(at)
	slot := &r.slots[slotIndex(start)]
	if slot.start == start {
		slot.state = statuspage.Worse(slot.state, s)
	} else {
		*slot = healthSlot{start: start, state: s}
	}
	r.current = s
}

// history returns the buckets of the last 24 hours, ending at the newest one
// that holds a sample. Ending there rather than at now keeps a bucket that
// opened seconds ago from reading as unknown until its first sample lands.
func (r *healthRing) history(now time.Time) statuspage.History {
	width := int64(statuspage.BucketWidth / time.Second)
	newest := statuspage.BucketStart(now)
	oldest := newest - width*(statuspage.HistoryBuckets-1)

	var h statuspage.History
	first := int64(-1)
	for start := oldest; start <= newest; start += width {
		if r.slots[slotIndex(start)].start == start {
			if first < 0 {
				first = start
			}
			h.End = start
		}
	}
	if first < 0 {
		return h
	}
	for start := first; start <= h.End; start += width {
		slot := r.slots[slotIndex(start)]
		if slot.start == start {
			h.States = append(h.States, slot.state)
		} else {
			h.States = append(h.States, statuspage.Unknown)
		}
	}
	return h
}

// healthLoop samples the services the report target lists. With no target
// the fleet has no status page, and probing on its behalf would be load for
// nobody.
func (a *Agent) healthLoop(ctx context.Context) {
	tick := time.NewTicker(HealthInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if t := a.ReportTarget(); t != nil {
			a.sampleHealth(ctx, t.Services)
		}
	}
}

// sampleHealth records one reading for each listed service on this host, and
// drops history for services no longer listed, so a service moved to `hide`
// stops being held as well as stops being sent.
func (a *Agent) sampleHealth(ctx context.Context, names []string) {
	at := a.clock()
	listed := map[string]bool{}

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return
		}
		s, ok := a.Service(name)
		if !ok {
			continue // listed fleet-wide, deployed elsewhere
		}
		listed[name] = true

		state := statuspage.Unknown
		if rt, err := RuntimeFor(s); err == nil {
			state = healthOf(ctx, rt, a.Target(s, ""))
		}
		a.recordHealth(name, at, state)
	}

	a.mu.Lock()
	for name := range a.health {
		if !listed[name] {
			delete(a.health, name)
		}
	}
	a.mu.Unlock()
}

// healthOf judges one service for the public page.
//
// The runtime speaks first. A failed Observe is unknown, not down: the agent
// failing to look is not the service failing. Degraded includes a oneshot
// awaiting its run, which the deploy gate rightly ignores and the public
// rightly does not. Only a running service is probed, and a failed probe
// makes it down, because the process being up is no comfort to someone
// getting errors from it.
func healthOf(ctx context.Context, rt runtime.Runtime, t *runtime.Target) statuspage.State {
	obs, err := rt.Observe(ctx, t)
	if err != nil {
		return statuspage.Unknown
	}

	switch {
	case obs.State == runtime.StateDegraded || obs.AwaitingRun:
		return statuspage.Degraded
	case obs.State == runtime.StateUnknown:
		return statuspage.Unknown
	case obs.State != runtime.StateRunning:
		return statuspage.Down
	}

	// A docker or systemd check is the observation just taken; probing would
	// only observe a second time.
	if h := t.Service.Health; h != nil && !h.Docker && !h.Systemd {
		if Probe(ctx, rt, t) != nil {
			return statuspage.Down
		}
	}
	return statuspage.Up
}

func (a *Agent) recordHealth(name string, at time.Time, s statuspage.State) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.health == nil {
		a.health = map[string]*healthRing{}
	}
	r := a.health[name]
	if r == nil {
		r = &healthRing{}
		a.health[name] = r
	}
	r.record(at, s)
}

// HealthReport returns the public state and history of each named service
// that has been sampled, in the order given.
func (a *Agent) HealthReport(names []string) []statuspage.ServiceReport {
	now := a.clock()
	a.mu.RLock()
	defer a.mu.RUnlock()

	var out []statuspage.ServiceReport
	for _, name := range names {
		r := a.health[name]
		if r == nil {
			continue
		}
		out = append(out, statuspage.ServiceReport{
			Name:    name,
			State:   r.current,
			History: r.history(now),
		})
	}
	return out
}

func (a *Agent) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now().UTC()
}
