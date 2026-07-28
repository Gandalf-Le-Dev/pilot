package agent

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/gandalfledev/pilot/internal/config"
	"github.com/gandalfledev/pilot/internal/edge/caddy"
	"github.com/gandalfledev/pilot/internal/release"
	"github.com/gandalfledev/pilot/internal/runtime"
	"github.com/gandalfledev/pilot/internal/transport/proto"
)

// jobTimeout bounds a single job, so a wedged health check or a hung docker
// command cannot leave a service in flight forever.
const jobTimeout = 30 * time.Minute

// StartDeploy queues an activation and returns immediately.
//
// The job runs in the daemon, detached from whoever asked for it. That is the
// whole point of phase 2: once the CLI has staged the bytes and handed over,
// closing a laptop mid-deploy can no longer leave a service activated but
// unverified with nobody to roll it back.
func (a *Agent) StartDeploy(req proto.DeployRequest) (*proto.Job, error) {
	if req.Release == "" || !release.IsID(req.Release) {
		return nil, fmt.Errorf("malformed release id %q", req.Release)
	}

	svc, err := a.PutService(req.Spec)
	if err != nil {
		return nil, fmt.Errorf("service definition: %w", err)
	}
	if svc.Name != req.Service {
		return nil, fmt.Errorf("spec is for %q but the request names %q", svc.Name, req.Service)
	}
	if !svc.Deployable() {
		return nil, fmt.Errorf("service %q is `manage: observe`", svc.Name)
	}
	if existing, busy := a.jobs.Active(svc.Name); busy {
		return nil, fmt.Errorf("%s already has job %s in flight", svc.Name, existing.ID)
	}

	rt, err := RuntimeFor(svc)
	if err != nil {
		return nil, err
	}

	t := a.Target(svc, req.Release)
	from, err := runtime.ReadCurrent(context.Background(), t)
	if err != nil {
		return nil, err
	}

	job := a.jobs.Create(proto.KindDeploy, svc.Name, req.Release, from)
	if svc.Rollout.IsBlueGreen() {
		go a.runBlueGreen(job.ID, svc, rt, req, from)
	} else {
		go a.runDeploy(job.ID, svc, rt, req, from)
	}
	return job, nil
}

// runDeploy is the state machine: activate, route, verify, and roll back if
// verification fails.
func (a *Agent) runDeploy(jobID string, svc *config.Service, rt runtime.Runtime, req proto.DeployRequest, from string) {
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()

	a.jobs.Start(jobID)
	started := time.Now().UTC()

	// One deploy per service at a time, enforced on disk rather than only in
	// memory: a restarted daemon or a stray CLI must not be able to interleave
	// two activations of the same service.
	unlock, err := a.lockService(svc.Name)
	if err != nil {
		a.jobs.Finish(jobID, err, false)
		return
	}
	defer unlock()

	t := a.Target(svc, req.Release)

	// ---- activate: the commit point.
	a.jobs.Event(jobID, proto.PhaseActivate, "activating %s", req.Release)
	if err := rt.Activate(ctx, t); err != nil {
		a.jobs.Event(jobID, proto.PhaseActivate, "activation failed: %v", err)
		rolled := a.rollback(ctx, jobID, svc, rt, from, req)
		a.record(ctx, t, svc, req, from, started, outcomeFor(rolled), err)
		a.jobs.Finish(jobID, err, rolled)
		return
	}

	// An updated route goes in with the code it belongs to; a brand new one
	// waits for the health check, so Caddy never points at something not ready.
	if req.RouteAction == proto.RouteUpdate && req.Route != "" {
		if err := a.installRoute(ctx, jobID, svc.Name, req.Route); err != nil {
			rolled := a.rollback(ctx, jobID, svc, rt, from, req)
			a.record(ctx, t, svc, req, from, started, outcomeFor(rolled), err)
			a.jobs.Finish(jobID, err, rolled)
			return
		}
	}

	// ---- verify: this is the loop that had to move onto the host.
	if req.Verify {
		a.jobs.Event(jobID, proto.PhaseVerify, "verifying health")
		if err := a.verify(ctx, jobID, rt, t); err != nil {
			a.jobs.Event(jobID, proto.PhaseVerify, "unhealthy: %v", err)
			rolled := a.rollback(ctx, jobID, svc, rt, from, req)
			a.record(ctx, t, svc, req, from, started, outcomeFor(rolled), err)
			a.jobs.Finish(jobID, err, rolled)
			return
		}
		a.jobs.Event(jobID, proto.PhaseVerify, "healthy")
	}

	if req.RouteAction == proto.RouteInstall && req.Route != "" {
		if err := a.installRoute(ctx, jobID, svc.Name, req.Route); err != nil {
			// The service is healthy; only its front door is missing. Rolling
			// back working code over a routing problem is the wrong trade.
			a.record(ctx, t, svc, req, from, started, release.OutcomeFailed, err)
			a.jobs.Finish(jobID, fmt.Errorf("service is healthy but its route could not be installed: %w", err), false)
			return
		}
	}

	// ---- finalise
	a.jobs.Event(jobID, proto.PhaseFinalise, "recording state")
	a.record(ctx, t, svc, req, from, started, release.OutcomeOK, nil)

	keep := req.KeepReleases
	if keep < 1 {
		keep = svc.KeepReleases
	}
	if removed, err := runtime.GC(ctx, t, keep, req.Release, from); err != nil {
		a.jobs.Event(jobID, proto.PhaseFinalise, "pruning old releases failed: %v", err)
	} else if len(removed) > 0 {
		a.jobs.Event(jobID, proto.PhaseFinalise, "pruned %d old release(s)", len(removed))
	}

	a.setDrift(svc.Name, nil) // a fresh deploy resets any recorded divergence
	a.jobs.Finish(jobID, nil, false)
}

// verify polls the service's health check until it passes or times out.
func (a *Agent) verify(ctx context.Context, jobID string, rt runtime.Runtime, t *runtime.Target) error {
	h := t.Service.Health
	if h == nil || h.Probes() == 0 {
		a.jobs.Event(jobID, proto.PhaseVerify, "no health check configured, skipping")
		return nil
	}

	timeout := h.Timeout.Duration()
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	interval := h.Interval.Duration()
	if interval <= 0 {
		interval = 3 * time.Second
	}

	deadline := time.Now().Add(timeout)
	var last error

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := Probe(ctx, rt, t); err == nil {
			return nil
		} else {
			last = err
		}

		if attempt == 1 || attempt%5 == 0 {
			a.jobs.Event(jobID, proto.PhaseVerify, "attempt %d: %v", attempt, last)
		}
		if time.Now().Add(interval).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("did not become healthy within %s: %w", timeout, last)
}

// rollback returns the service to its previous release, restoring the route
// that shipped with it so code and routing move back together.
func (a *Agent) rollback(ctx context.Context, jobID string, svc *config.Service, rt runtime.Runtime, from string, req proto.DeployRequest) bool {
	if from == "" {
		a.jobs.Event(jobID, proto.PhaseRollback, "nothing to roll back to — this was the first release")
		return false
	}

	a.jobs.Event(jobID, proto.PhaseRollback, "rolling back to %s", from)
	prev := a.Target(svc, from)

	if err := rt.Activate(ctx, prev); err != nil {
		// This is the genuinely bad case, and it must be unmissable in the log.
		a.jobs.Event(jobID, proto.PhaseRollback, "ROLLBACK FAILED: %v", err)
		a.jobs.Event(jobID, proto.PhaseRollback, "the service may be down — investigate before deploying again")
		return false
	}

	if req.Route != "" {
		body, err := os.ReadFile(a.Layout.Snippet(svc.Name, from))
		if err == nil {
			if _, err := caddy.InstallSnippet(ctx, a.Exec, a.Caddy, svc.Name, string(body)); err != nil {
				a.jobs.Event(jobID, proto.PhaseRollback, "route rollback failed: %v", err)
			}
		}
	}

	a.jobs.Event(jobID, proto.PhaseRollback, "rolled back to %s", from)
	return true
}

func (a *Agent) installRoute(ctx context.Context, jobID, service, snippet string) error {
	action, err := caddy.InstallSnippet(ctx, a.Exec, a.Caddy, service, snippet)
	if err != nil {
		a.jobs.Event(jobID, proto.PhaseRoute, "route failed: %v", err)
		return err
	}
	if action != caddy.ActionNone {
		a.jobs.Event(jobID, proto.PhaseRoute, "route %s", action)
	}
	return nil
}

// record updates the service's state.json.
func (a *Agent) record(ctx context.Context, t *runtime.Target, svc *config.Service, req proto.DeployRequest, from string, started time.Time, outcome release.Outcome, cause error) {
	st, err := runtime.ReadState(ctx, t)
	if err != nil {
		st = release.NewState(svc.Name)
	}

	switch outcome {
	case release.OutcomeOK:
		st.Promote(req.Release)
	case release.OutcomeRolledBack:
		if from != "" {
			st.Current = from
		}
	}

	rec := release.DeployRecord{
		Release: req.Release, From: from, Host: a.Host, By: req.By,
		StartedAt: started, FinishedAt: time.Now().UTC(), Outcome: outcome,
	}
	if cause != nil {
		rec.Reason = firstLine(cause.Error())
	}
	st.Record(rec)

	if err := runtime.WriteState(ctx, t, st); err != nil {
		logf("could not record state for %s: %v", svc.Name, err)
	}
}

func outcomeFor(rolledBack bool) release.Outcome {
	if rolledBack {
		return release.OutcomeRolledBack
	}
	return release.OutcomeFailed
}

// StartRollback queues a manual rollback.
func (a *Agent) StartRollback(req proto.RollbackRequest) (*proto.Job, error) {
	svc, ok := a.Service(req.Service)
	if !ok {
		return nil, fmt.Errorf("no such service %q on this host", req.Service)
	}
	if !svc.Deployable() {
		return nil, fmt.Errorf("service %q is `manage: observe`", svc.Name)
	}
	if existing, busy := a.jobs.Active(svc.Name); busy {
		return nil, fmt.Errorf("%s already has job %s in flight", svc.Name, existing.ID)
	}

	rt, err := RuntimeFor(svc)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	t := a.Target(svc, "")
	st, err := runtime.ReadState(ctx, t)
	if err != nil {
		return nil, err
	}
	if _, err := runtime.Reconcile(ctx, t, st); err != nil {
		return nil, err
	}

	to := req.To
	if to == "" {
		if to, err = st.RollbackTarget(); err != nil {
			return nil, err
		}
	}
	if to == st.Current {
		return nil, fmt.Errorf("%s already runs %s", svc.Name, to)
	}
	if ok, _ := a.Exec.Exists(ctx, a.Layout.Release(svc.Name, to)); !ok {
		return nil, fmt.Errorf("release %s is no longer on disk", to)
	}

	job := a.jobs.Create(proto.KindRollback, svc.Name, to, st.Current)
	if svc.Rollout.IsBlueGreen() {
		// A blue-green rollback is the same dance in reverse: stand the target
		// release up on the idle colour, verify it, then move the route. Going
		// through the plain path would try to `up -d` a third, unrouted stack.
		go a.runBlueGreen(job.ID, svc, rt, proto.DeployRequest{
			Service: svc.Name, Release: to, Verify: true, By: req.By,
			KeepReleases: svc.KeepReleases,
		}, st.Current)
		return job, nil
	}
	go a.runRollback(job.ID, svc, rt, to, st.Current, req.By)
	return job, nil
}

func (a *Agent) runRollback(jobID string, svc *config.Service, rt runtime.Runtime, to, from, by string) {
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()

	a.jobs.Start(jobID)
	started := time.Now().UTC()

	unlock, err := a.lockService(svc.Name)
	if err != nil {
		a.jobs.Finish(jobID, err, false)
		return
	}
	defer unlock()

	t := a.Target(svc, to)
	a.jobs.Event(jobID, proto.PhaseActivate, "activating %s", to)
	if err := rt.Activate(ctx, t); err != nil {
		a.jobs.Finish(jobID, err, false)
		return
	}

	if svc.Expose != nil {
		if body, err := os.ReadFile(a.Layout.Snippet(svc.Name, to)); err == nil {
			if err := a.installRoute(ctx, jobID, svc.Name, string(body)); err != nil {
				a.jobs.Event(jobID, proto.PhaseRoute, "route restore failed: %v", err)
			}
		}
	}

	a.jobs.Event(jobID, proto.PhaseVerify, "verifying health")
	if err := a.verify(ctx, jobID, rt, t); err != nil {
		err = fmt.Errorf("rolled back to %s but it is not healthy either: %w", to, err)
		a.jobs.Finish(jobID, err, false)
		return
	}

	st, err := runtime.ReadState(ctx, t)
	if err != nil {
		st = release.NewState(svc.Name)
	}
	st.Promote(to)
	st.Record(release.DeployRecord{
		Release: to, From: from, Host: a.Host, By: by,
		StartedAt: started, FinishedAt: time.Now().UTC(),
		Outcome: release.OutcomeOK, Reason: "manual rollback",
	})
	if err := runtime.WriteState(ctx, t, st); err != nil {
		logf("could not record state for %s: %v", svc.Name, err)
	}

	a.setDrift(svc.Name, nil)
	a.jobs.Finish(jobID, nil, false)
}

// lockService takes an exclusive flock on the service directory.
//
// An advisory lock on a real file, rather than a mutex, because it also holds
// against a separate process — a CLI running in degraded mode, or a second
// daemon during an upgrade.
func (a *Agent) lockService(name string) (func(), error) {
	dir := a.Layout.Service(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	path := a.Layout.Lock(name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s is locked by another deploy", name)
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
