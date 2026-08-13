package agent

import (
	"context"
	"os"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
)

// Loop intervals.
const (
	// DriftInterval is how often live configuration is compared against the
	// manifest. Five minutes is frequent enough to catch a 2am hand-edit before
	// the next deploy silently reverts it, and cheap enough to leave on.
	DriftInterval = 5 * time.Minute

	// ObserveInterval is how often service state is sampled.
	ObserveInterval = 10 * time.Second
)

// StartLoops launches the agent's background work. It returns immediately; the
// loops stop when ctx is cancelled.
func (a *Agent) StartLoops(ctx context.Context) {
	go a.driftLoop(ctx)
	go a.observeLoop(ctx)
	go a.alertLoop(ctx)
}

// driftLoop periodically compares what is running against what should be.
func (a *Agent) driftLoop(ctx context.Context) {
	// A short initial delay lets the socket come up first, so an immediate
	// `pilot status` isn't queued behind a full fingerprint sweep.
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}

	tick := time.NewTicker(DriftInterval)
	defer tick.Stop()

	for {
		a.checkDrift(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// checkDrift fingerprints every service and records divergence.
func (a *Agent) checkDrift(ctx context.Context) {
	for _, name := range a.ServiceNames() {
		if err := ctx.Err(); err != nil {
			return
		}
		// A deploy in flight is *expected* to look different mid-flight;
		// reporting that as drift would be pure noise.
		if _, busy := a.jobs.Active(name); busy {
			continue
		}
		a.checkServiceDrift(ctx, name)
	}
}

func (a *Agent) checkServiceDrift(ctx context.Context, name string) {
	s, ok := a.Service(name)
	if !ok {
		return
	}
	rt, err := RuntimeFor(s)
	if err != nil {
		return
	}

	t := a.Target(s, "")
	current, err := runtime.ReadCurrent(ctx, t)
	if err != nil || current == "" {
		a.setDrift(name, nil)
		return
	}

	rec := &driftRecord{detectedAt: time.Now().UTC(), release: current}

	// Config drift: the runtime's live fingerprint versus the one implied by
	// the release. The manifest is the record of what this release claims to
	// be, so a mismatch means something changed underneath it.
	if fp, err := rt.Fingerprint(ctx, t); err == nil {
		if recorded, err := a.readFingerprint(name, current); err == nil {
			if recorded != "" && recorded != fp {
				rec.config = true
				rec.detail = "live configuration differs from the deployed release"
			}
		} else {
			// First observation after a deploy: record the baseline rather
			// than reporting drift against nothing.
			_ = a.writeFingerprint(name, current, fp)
		}
	}

	// Route drift: the installed snippet versus the copy that shipped with the
	// release.
	if s.Expose != nil {
		want, err := os.ReadFile(a.Layout.Snippet(name, current))
		if err == nil {
			got, err := os.ReadFile(caddySnippetPath(a.Caddy, name))
			if err != nil || string(got) != string(want) {
				rec.route = true
				if rec.detail == "" {
					rec.detail = "installed route differs from the one deployed with this release"
				} else {
					rec.detail += "; route also differs"
				}
			}
		}
	}

	if !rec.config && !rec.route {
		a.setDrift(name, nil)
		return
	}
	a.setDrift(name, rec)
}

// fingerprintPath is where the baseline for a release is cached.
//
// It lives beside the release rather than in memory so that a daemon restart
// does not re-baseline and thereby forget drift it had already found. The
// name is the shared constant because the static runtime fingerprints the
// release tree and has to know to exclude this file from it.
func (a *Agent) fingerprintPath(service, releaseID string) string {
	return a.Layout.Release(service, releaseID) + "/" + release.FingerprintFile
}

func (a *Agent) readFingerprint(service, releaseID string) (string, error) {
	b, err := os.ReadFile(a.fingerprintPath(service, releaseID))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (a *Agent) writeFingerprint(service, releaseID, fp string) error {
	return release.WriteFileAtomic(a.fingerprintPath(service, releaseID), []byte(fp), 0o644)
}

func caddySnippetPath(p caddy.Paths, service string) string {
	return p.SnippetDir + "/" + caddy.SnippetName(service)
}

// observeLoop samples service state so a status query is answered from a recent
// reading rather than a burst of fresh shell-outs.
func (a *Agent) observeLoop(ctx context.Context) {
	tick := time.NewTicker(ObserveInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		for _, name := range a.ServiceNames() {
			if err := ctx.Err(); err != nil {
				return
			}
			obs, err := a.Observe(ctx, name)
			if err != nil {
				continue
			}
			a.recordSample(name, obs)
		}
	}
}

// Sample is one reading of a service's state.
type Sample struct {
	At    time.Time     `json:"at"`
	State runtime.State `json:"state"`
}

// recordSample appends to the in-memory history for a service.
func (a *Agent) recordSample(name string, obs runtime.Observation) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.samples == nil {
		a.samples = map[string][]Sample{}
	}
	s := append(a.samples[name], Sample{At: time.Now().UTC(), State: obs.State})
	if len(s) > maxSamples {
		s = s[len(s)-maxSamples:]
	}
	a.samples[name] = s
}

// maxSamples bounds the in-memory history. At ObserveInterval this is roughly
// an hour, which is enough to answer "is it flapping right now".
const maxSamples = 360

// Samples returns the recent readings for a service.
func (a *Agent) Samples(name string) []Sample {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]Sample(nil), a.samples[name]...)
}
