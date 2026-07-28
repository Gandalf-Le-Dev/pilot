package agent

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime/compose"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// runBlueGreen deploys by standing the new version up beside the old one and
// moving the route once it is proven healthy.
//
// The shape of this function is the whole argument for the strategy: nothing
// user-visible happens until step 4, so every way it can fail before then costs
// the user precisely nothing. Compare `recreate`, where a bad release means one
// restart to deploy it and a second to back it out, both of them visible.
func (a *Agent) runBlueGreen(jobID string, svc *config.Service, rt runtime.Runtime, req proto.DeployRequest, from string) {
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

	active := a.activeColor(svc.Name)
	target := active.Other()
	newRelease := a.Target(svc, req.Release)

	a.jobs.Event(jobID, proto.PhaseActivate, "active is %s; bringing up %s on port %d",
		active, target, svc.Rollout.PortFor(target))

	// ---- 1. start the idle color. It takes no traffic.
	if err := compose.StartColor(ctx, newRelease, target, newRelease.ReleaseDir()); err != nil {
		a.jobs.Event(jobID, proto.PhaseActivate, "%s failed to start: %v", target, err)
		a.abandonColor(ctx, jobID, newRelease, target)
		a.record(ctx, newRelease, svc, req, from, started, release.OutcomeFailed, err)
		// Nothing was ever flipped, so the live service is untouched. This is
		// a failure with zero user impact, and it should read that way.
		a.jobs.Event(jobID, proto.PhaseDone, "%s is still serving — no user impact", active)
		a.jobs.Finish(jobID, err, false)
		return
	}

	// ---- 2. verify the new color on its own port, before anyone can reach it.
	if req.Verify {
		a.jobs.Event(jobID, proto.PhaseVerify, "checking %s directly on port %d",
			target, svc.Rollout.PortFor(target))

		if err := a.verifyColor(ctx, jobID, rt, newRelease, target); err != nil {
			a.jobs.Event(jobID, proto.PhaseVerify, "%s is unhealthy: %v", target, err)
			a.abandonColor(ctx, jobID, newRelease, target)
			a.record(ctx, newRelease, svc, req, from, started, release.OutcomeFailed, err)
			a.jobs.Event(jobID, proto.PhaseDone, "%s is still serving — no user impact", active)
			a.jobs.Finish(jobID, err, false)
			return
		}
		a.jobs.Event(jobID, proto.PhaseVerify, "%s is healthy", target)
	}

	// ---- 3. move the symlink. The route still points at the old color, so
	// this only changes what `current` names, not what serves.
	if err := runtime.Swap(ctx, newRelease); err != nil {
		a.abandonColor(ctx, jobID, newRelease, target)
		a.record(ctx, newRelease, svc, req, from, started, release.OutcomeFailed, err)
		a.jobs.Finish(jobID, err, false)
		return
	}

	// ---- 4. the flip. A Caddy reload is graceful, so in-flight requests
	// finish against the old upstream and new ones go to the new color.
	snippet := req.Route
	if snippet == "" {
		snippet, err = a.renderColorRoute(svc, target)
		if err != nil {
			a.jobs.Finish(jobID, err, false)
			return
		}
	}

	a.jobs.Event(jobID, proto.PhaseRoute, "flipping traffic to %s", target)
	if err := a.installRoute(ctx, jobID, svc.Name, snippet); err != nil {
		// The route never moved, so the old color is still serving. Undo the
		// symlink and drop the new color.
		a.jobs.Event(jobID, proto.PhaseRollback, "flip failed; %s never stopped serving", active)
		if from != "" {
			prev := a.Target(svc, from)
			_ = runtime.Swap(ctx, prev)
		}
		a.abandonColor(ctx, jobID, newRelease, target)
		a.record(ctx, newRelease, svc, req, from, started, release.OutcomeFailed, err)
		a.jobs.Finish(jobID, err, false)
		return
	}

	a.setActiveColor(svc.Name, target)

	// ---- 5. drain. Both versions are live during this window, which is the
	// operational tax of the strategy and the reason schema changes have to be
	// backward compatible across it.
	drain := svc.Rollout.DrainOrDefault().Duration()
	if drain > 0 {
		a.jobs.Event(jobID, proto.PhaseFinalise, "draining %s for %s", active, drain)
		select {
		case <-ctx.Done():
		case <-time.After(drain):
		}
	}

	// ---- 6. stop the old color. Only after this does a rollback cost a
	// restart; until now it was a snippet rewrite.
	oldDir := a.Layout.Release(svc.Name, from)
	if from == "" {
		oldDir = newRelease.ReleaseDir()
	}
	if err := compose.StopColor(ctx, newRelease, active, oldDir); err != nil {
		// Not fatal: the new version is serving. A lingering old stack costs
		// memory, not correctness.
		a.jobs.Event(jobID, proto.PhaseFinalise, "could not stop %s: %v", active, err)
	} else {
		a.jobs.Event(jobID, proto.PhaseFinalise, "stopped %s", active)
	}

	a.record(ctx, newRelease, svc, req, from, started, release.OutcomeOK, nil)

	keep := req.KeepReleases
	if keep < 1 {
		keep = svc.KeepReleases
	}
	if removed, err := runtime.GC(ctx, newRelease, keep, req.Release, from); err == nil && len(removed) > 0 {
		a.jobs.Event(jobID, proto.PhaseFinalise, "pruned %d old release(s)", len(removed))
	}

	a.setDrift(svc.Name, nil)
	a.jobs.Finish(jobID, nil, false)
}

// verifyColor health-checks a specific color on its own host port.
//
// The service's configured health check names a port; for blue-green that port
// belongs to whichever color is active, so it is rewritten to the color under
// test. Checking through the public URL instead would test the color that is
// already live — which would pass, and mean nothing.
func (a *Agent) verifyColor(ctx context.Context, jobID string, rt runtime.Runtime, t *runtime.Target, c config.Color) error {
	scoped := *t.Service
	scoped.Health = rewriteHealthPort(t.Service.Health, t.Service.Rollout.PortFor(c))

	probeTarget := *t
	probeTarget.Service = &scoped

	return a.verify(ctx, jobID, rt, &probeTarget)
}

// rewriteHealthPort points a health check at a specific port.
func rewriteHealthPort(h *config.Health, port int) *config.Health {
	if h == nil || port == 0 {
		return h
	}
	out := *h

	switch {
	case h.HTTP != nil:
		u := *h.HTTP
		u.URL = replacePort(u.URL, port)
		out.HTTP = &u
	case h.TCP != nil:
		p := *h.TCP
		p.Addr = fmt.Sprintf("127.0.0.1:%d", port)
		out.TCP = &p
	case h.Docker:
		// Container-level health is already per-color, since each color is its
		// own compose project. Nothing to rewrite.
	}
	return &out
}

// replacePort swaps the port in an http(s) URL.
func replacePort(raw string, port int) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Host = fmt.Sprintf("%s:%d", u.Hostname(), port)
	return u.String()
}

// abandonColor tears down a color that never took traffic.
func (a *Agent) abandonColor(ctx context.Context, jobID string, t *runtime.Target, c config.Color) {
	if err := compose.StopColor(ctx, t, c, t.ReleaseDir()); err != nil {
		a.jobs.Event(jobID, proto.PhaseRollback, "could not clean up %s: %v", c, err)
		return
	}
	a.jobs.Event(jobID, proto.PhaseRollback, "removed the %s stack", c)
}

// renderColorRoute produces the Caddy snippet pointing at a color's port.
func (a *Agent) renderColorRoute(svc *config.Service, c config.Color) (string, error) {
	if svc.Expose == nil {
		return "", fmt.Errorf("service %q has no route to flip", svc.Name)
	}
	scoped := *svc.Expose
	scoped.Upstream = svc.Rollout.PortFor(c)

	return caddy.Render(caddy.Input{
		Service: svc.Name,
		Expose:  &scoped,
		Root:    a.Layout.Current(svc.Name),
	})
}

// activeColor reports which color is currently serving.
//
// It is read from state.json rather than inferred from the installed route,
// because a hand-edited route should show up as drift rather than silently
// redirect the next deploy.
func (a *Agent) activeColor(service string) config.Color {
	st, err := release.ReadState(a.Layout.Service(service), service)
	if err != nil || st.ActiveColor == "" {
		return config.ColorBlue
	}
	return config.Color(st.ActiveColor)
}

func (a *Agent) setActiveColor(service string, c config.Color) {
	dir := a.Layout.Service(service)
	st, err := release.ReadState(dir, service)
	if err != nil {
		st = release.NewState(service)
	}
	st.ActiveColor = string(c)
	if err := release.WriteState(dir, st); err != nil {
		logf("could not record active color for %s: %v", service, err)
	}
}
