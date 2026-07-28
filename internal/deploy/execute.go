package deploy

import (
	"context"
	"fmt"
	"time"

	"github.com/gandalfledev/pilot/internal/agent/remote"
	"github.com/gandalfledev/pilot/internal/edge/caddy"
	"github.com/gandalfledev/pilot/internal/release"
	"github.com/gandalfledev/pilot/internal/runtime"
)

// Logger receives progress messages.
type Logger func(format string, args ...any)

// Executor runs a plan.
type Executor struct {
	Runtime runtime.Runtime
	Targets map[string]*runtime.Target
	Caddy   caddy.Paths
	Env     map[string]string
	Log     Logger

	// Agents holds the agent client for each host that has one. A host absent
	// from this map is driven directly over SSH instead.
	Agents map[string]*remote.Client

	// Spec is the service definition as YAML, handed to the agent so it can go
	// on observing, probing, and alerting on this service with no CLI present.
	Spec string

	// By identifies who asked, for the deploy record.
	By string

	// SkipVerify disables health verification, and with it the automatic
	// rollback that depends on it.
	SkipVerify bool
}

// Outcome records what happened on one host.
type Outcome struct {
	Host       string `json:"host"`
	Release    string `json:"release"`
	From       string `json:"from,omitempty"`
	Succeeded  bool   `json:"succeeded"`
	RolledBack bool   `json:"rolled_back,omitempty"`
	Err        error  `json:"-"`
	Message    string `json:"message,omitempty"`
}

// Run executes the plan host by host.
//
// Hosts are processed serially by default with a fail-fast gate: for a
// single-operator fleet, finding out on host 1 and leaving hosts 2..n untouched
// is almost always what you want.
func (e *Executor) Run(ctx context.Context, p *Plan) ([]Outcome, error) {
	var outcomes []Outcome

	maxUnhealthy := 0
	if p.Service.Rollout != nil {
		maxUnhealthy = p.Service.Rollout.MaxUnhealthy
	}

	failures := 0
	for i, hp := range p.Hosts {
		if err := ctx.Err(); err != nil {
			return outcomes, err
		}

		if hp.NoOp {
			e.Log("  %s: already running %s, nothing to do", hp.Host, hp.To)
			outcomes = append(outcomes, Outcome{
				Host: hp.Host, Release: hp.To, From: hp.From,
				Succeeded: true, Message: "unchanged",
			})
			continue
		}

		out := e.deployHost(ctx, p, hp)
		outcomes = append(outcomes, out)

		if !out.Succeeded {
			failures++
			if failures > maxUnhealthy {
				if remaining := len(p.Hosts) - i - 1; remaining > 0 {
					e.Log("  aborting: %d host(s) not attempted", remaining)
				}
				return outcomes, fmt.Errorf("deploy failed on %s: %w", out.Host, out.Err)
			}
		}

		if p.Service.Rollout != nil && i < len(p.Hosts)-1 {
			if pause := p.Service.Rollout.PauseBetween.Duration(); pause > 0 {
				select {
				case <-ctx.Done():
					return outcomes, ctx.Err()
				case <-time.After(pause):
				}
			}
		}
	}
	return outcomes, nil
}

// deployHost runs the pipeline for one host: stage, activate, verify, and roll
// back if verification fails.
func (e *Executor) deployHost(ctx context.Context, p *Plan, hp HostPlan) Outcome {
	out := Outcome{Host: hp.Host, Release: hp.To, From: hp.From}

	t, ok := e.Targets[hp.Host]
	if !ok {
		out.Err = fmt.Errorf("host %s is unreachable", hp.Host)
		return out
	}
	t.Release = p.Release

	started := now()

	// ---- stage: everything that can fail should fail here, with the running
	// service still untouched.
	e.Log("  %s: staging %s", hp.Host, p.Release)
	stageIn := runtime.StageInput{
		LocalDir: p.LocalDir,
		Env:      e.Env,
		Manifest: p.Manifest(hp.Host),
		Snippet:  p.Snippet,
	}
	if err := e.Runtime.Stage(ctx, t, stageIn); err != nil {
		out.Err = err
		e.recordFailure(ctx, t, p, hp, started, release.OutcomeFailed, err)
		return out
	}

	// A route that Caddy would reject must be caught before activation, not
	// after: this is the same class of failure as a bad image pull.
	if p.Snippet != "" && hp.Route != RouteNone {
		if err := caddy.Validate(ctx, t.Client, e.Caddy); err != nil {
			out.Err = fmt.Errorf("existing caddy config is already invalid, refusing to reload: %w", err)
			e.recordFailure(ctx, t, p, hp, started, release.OutcomeFailed, out.Err)
			return out
		}
	}

	// ---- activate. When the host has an agent, hand the job over: from here
	// on it runs in the daemon, so losing this connection stops the watching,
	// not the deploy. Without an agent the CLI drives it, and says so.
	if rc := e.Agents[hp.Host]; rc != nil {
		return e.activateViaAgent(ctx, rc, p, hp, started, out)
	}

	e.Log("  %s: no agent — activating over SSH; if this connection drops now,", hp.Host)
	e.Log("    nothing will finish the verification or the rollback (`pilot bootstrap %s`)", hp.Host)

	e.Log("  %s: activating", hp.Host)
	if err := e.Runtime.Activate(ctx, t); err != nil {
		out.Err = err
		e.rollback(ctx, t, p, hp, "activation failed")
		out.RolledBack = hp.From != ""
		e.recordFailure(ctx, t, p, hp, started, release.OutcomeRolledBack, err)
		return out
	}

	// An updated route goes in now, alongside the code it belongs to. A brand
	// new route waits until the service is verified healthy, so Caddy never
	// points at something that isn't ready.
	if hp.Route == RouteUpdate {
		if err := e.installRoute(ctx, t, p); err != nil {
			out.Err = err
			e.rollback(ctx, t, p, hp, "route update failed")
			out.RolledBack = hp.From != ""
			e.recordFailure(ctx, t, p, hp, started, release.OutcomeRolledBack, err)
			return out
		}
	}

	// ---- verify
	if !e.SkipVerify {
		if err := Verify(ctx, e.Runtime, t, e.Log); err != nil {
			out.Err = err
			e.rollback(ctx, t, p, hp, "health check failed")
			out.RolledBack = hp.From != ""
			e.recordFailure(ctx, t, p, hp, started, release.OutcomeRolledBack, err)
			return out
		}
	}

	if hp.Route == RouteInstall {
		if err := e.installRoute(ctx, t, p); err != nil {
			// The service is healthy; only its front door is missing. Rolling
			// back working code over a routing problem would be the wrong
			// trade, so report it and leave the deploy standing.
			out.Err = fmt.Errorf("service is healthy but its route could not be installed: %w", err)
			e.recordFailure(ctx, t, p, hp, started, release.OutcomeFailed, out.Err)
			return out
		}
	}

	e.commit(ctx, t, p, hp, started)
	out.Succeeded = true
	return out
}

func (e *Executor) installRoute(ctx context.Context, t *runtime.Target, p *Plan) error {
	action, err := caddy.InstallSnippet(ctx, t.Client, e.Caddy, p.Service.Name, p.Snippet)
	if err != nil {
		return err
	}
	if action != caddy.ActionNone {
		e.Log("  %s: route %s (%s)", t.Host.Name, action, p.Service.Expose.Domains[0])
	}
	return nil
}

// rollback returns the host to the release it was running.
func (e *Executor) rollback(ctx context.Context, t *runtime.Target, p *Plan, hp HostPlan, why string) {
	if hp.From == "" {
		e.Log("  %s: %s — nothing to roll back to (this was the first release)", t.Host.Name, why)
		return
	}

	e.Log("  %s: %s — rolling back to %s", t.Host.Name, why, hp.From)

	prev := *t
	prev.Release = hp.From
	if err := e.Runtime.Activate(ctx, &prev); err != nil {
		e.Log("  %s: ROLLBACK FAILED: %v", t.Host.Name, err)
		e.Log("  %s: the service may be down — investigate before deploying again", t.Host.Name)
		return
	}

	// Routing rolls back with the code, from the copy kept in the previous
	// release directory.
	if p.Snippet != "" && hp.Route != RouteNone {
		body, err := t.Client.ReadFile(ctx, t.Layout.Snippet(p.Service.Name, hp.From))
		if err == nil {
			if _, err := caddy.InstallSnippet(ctx, t.Client, e.Caddy, p.Service.Name, string(body)); err != nil {
				e.Log("  %s: route rollback failed: %v", t.Host.Name, err)
			}
		}
	}
	e.Log("  %s: rolled back to %s", t.Host.Name, hp.From)
}

// commit records the successful deploy and prunes superseded releases.
func (e *Executor) commit(ctx context.Context, t *runtime.Target, p *Plan, hp HostPlan, started time.Time) {
	st, err := runtime.ReadState(ctx, t)
	if err != nil {
		st = release.NewState(p.Service.Name)
	}
	st.Promote(p.Release)
	st.Record(release.DeployRecord{
		Release: p.Release, From: hp.From, Host: hp.Host, By: deployer(),
		StartedAt: started, FinishedAt: now(), Outcome: release.OutcomeOK,
	})
	if err := runtime.WriteState(ctx, t, st); err != nil {
		e.Log("  %s: could not record state: %v", hp.Host, err)
	}

	keep := p.Service.KeepReleases
	if keep < 1 {
		keep = 5
	}
	removed, err := runtime.GC(ctx, t, keep, st.Protected()...)
	if err != nil {
		e.Log("  %s: pruning old releases failed: %v", hp.Host, err)
		return
	}
	if len(removed) > 0 {
		e.Log("  %s: pruned %d old release(s)", hp.Host, len(removed))
	}
}

func (e *Executor) recordFailure(ctx context.Context, t *runtime.Target, p *Plan, hp HostPlan, started time.Time, outcome release.Outcome, cause error) {
	st, err := runtime.ReadState(ctx, t)
	if err != nil {
		st = release.NewState(p.Service.Name)
	}
	if outcome == release.OutcomeRolledBack && hp.From != "" {
		st.Current = hp.From
	}
	st.Record(release.DeployRecord{
		Release: p.Release, From: hp.From, Host: hp.Host, By: deployer(),
		StartedAt: started, FinishedAt: now(), Outcome: outcome,
		Reason: firstLine(cause.Error()),
	})
	_ = runtime.WriteState(ctx, t, st)
}

// Rollback activates an earlier release, restoring its route alongside it.
func Rollback(ctx context.Context, rt runtime.Runtime, t *runtime.Target, paths caddy.Paths, to string, log Logger) error {
	st, err := runtime.ReadState(ctx, t)
	if err != nil {
		return err
	}
	if _, err := runtime.Reconcile(ctx, t, st); err != nil {
		return err
	}

	if to == "" {
		if to, err = st.RollbackTarget(); err != nil {
			return err
		}
	}
	if to == st.Current {
		return fmt.Errorf("%s already runs %s", t.Label(), to)
	}

	from := st.Current
	started := now()

	t.Release = to
	log("  %s: activating %s", t.Host.Name, to)
	if err := rt.Activate(ctx, t); err != nil {
		return err
	}

	if t.Service.Expose != nil {
		body, err := t.Client.ReadFile(ctx, t.Layout.Snippet(t.Service.Name, to))
		if err == nil {
			action, err := caddy.InstallSnippet(ctx, t.Client, paths, t.Service.Name, string(body))
			if err != nil {
				log("  %s: route rollback failed: %v", t.Host.Name, err)
			} else if action != caddy.ActionNone {
				log("  %s: route %s", t.Host.Name, action)
			}
		}
	}

	if err := Verify(ctx, rt, t, log); err != nil {
		return fmt.Errorf("rolled back to %s but it is not healthy either: %w", to, err)
	}

	st.Promote(to)
	st.Record(release.DeployRecord{
		Release: to, From: from, Host: t.Host.Name, By: deployer(),
		StartedAt: started, FinishedAt: now(), Outcome: release.OutcomeOK,
		Reason: "manual rollback",
	})
	return runtime.WriteState(ctx, t, st)
}
