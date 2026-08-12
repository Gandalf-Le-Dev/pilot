package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// now is overridable in tests.
var now = func() time.Time { return time.Now().UTC() }

// Verify polls a service's health check until it passes or the timeout expires.
//
// In this build the polling runs from the operator's machine. That is the one
// place phase 1 is weaker than the design intends: if the laptop disconnects
// mid-verify, nothing completes the rollback. The agent takes this over in
// phase 2, at which point the loop moves to the host and survives disconnection.
func Verify(ctx context.Context, rt runtime.Runtime, t *runtime.Target, log Logger) error {
	h := t.Service.Health
	if h == nil || h.Probes() == 0 {
		log("  %s: no health check configured, skipping verification", t.Host.Name)
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
	var lastErr error

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := probe(ctx, rt, t, h)
		if err == nil {
			log("  %s: healthy after %d attempt(s)", t.Host.Name, attempt)
			return nil
		}
		lastErr = err

		if time.Now().Add(interval).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}

	return fmt.Errorf("%s did not become healthy within %s: %w", t.Label(), timeout, lastErr)
}

// probe runs one health check.
func probe(ctx context.Context, rt runtime.Runtime, t *runtime.Target, h *config.Health) error {
	switch {
	case h.HTTP != nil:
		return probeHTTP(ctx, t, h.HTTP)
	case h.TCP != nil:
		return probeTCP(ctx, t, h.TCP)
	case h.Exec != nil:
		return probeExec(ctx, t, h.Exec)
	case h.Docker:
		return probeDocker(ctx, rt, t)
	case h.Systemd:
		return probeUnit(ctx, rt, t)
	}
	return nil
}

// probeHTTP curls the endpoint from the host, so a check against localhost
// means what it says.
func probeHTTP(ctx context.Context, t *runtime.Target, p *config.HTTPProbe) error {
	cmd := transport.Join("curl", "--silent", "--show-error", "--max-time", "10",
		"--output", "/dev/null", "--write-out", "%{http_code}", p.URL)

	res, err := t.Client.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("GET %s failed: %s", p.URL, firstLine(res.Stderr))
	}

	got := res.Out()
	want := fmt.Sprint(p.Expect)
	if got != want {
		return fmt.Errorf("GET %s returned %s, want %s", p.URL, got, want)
	}
	return nil
}

func probeTCP(ctx context.Context, t *runtime.Target, p *config.TCPProbe) error {
	host, port, ok := strings.Cut(p.Addr, ":")
	if !ok {
		return fmt.Errorf("malformed tcp address %q, want host:port", p.Addr)
	}

	// bash's /dev/tcp works without nc being installed, which it often isn't
	// on a minimal server image.
	cmd := fmt.Sprintf("timeout 5 bash -c %s",
		transport.Quote(fmt.Sprintf("exec 3<>/dev/tcp/%s/%s", host, port)))

	res, err := t.Client.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("cannot connect to %s", p.Addr)
	}
	return nil
}

// probeExec runs a command on the host.
//
// It runs on the host rather than inside a container, deliberately: guessing
// which container of a multi-service stack to enter would be wrong as often as
// right. To probe inside one, name it — `docker compose -p api exec -T db
// pg_isready` is unambiguous.
func probeExec(ctx context.Context, t *runtime.Target, p *config.ExecProbe) error {
	res, err := t.Client.Run(ctx, transport.Join(p.Cmd...))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("%s exited %d: %s", p.Cmd[0], res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// probeUnit asks systemd, via the runtime's Observe.
//
// Delegating rather than running `systemctl is-active` here is what makes this
// probe mean the right thing for both unit kinds. A oneshot is never "active" —
// it runs and exits — so is-active would report every backup job as unhealthy
// forever. Observe already knows that a oneshot's health is whether its last
// run succeeded and how recently, and this inherits that for free.
func probeUnit(ctx context.Context, rt runtime.Runtime, t *runtime.Target) error {
	obs, err := rt.Observe(ctx, t)
	if err != nil {
		return err
	}
	// A state that judges a past run is not a verdict on this release. A
	// scheduled job that has never run, or whose last success predates its
	// freshness bound, cannot be made healthy by deploying — so gating on it
	// rolls back every fix pushed to a service that was already stale,
	// including the fix for whatever stopped it running.
	//
	// Alerting still reports these as degraded, which is what the operator
	// wants to hear. Only the gate ignores them, because only the gate is
	// asking a question the state cannot answer.
	if obs.AwaitingRun {
		return nil
	}
	if obs.State != runtime.StateRunning {
		detail := obs.Detail
		if detail == "" {
			detail = string(obs.State)
		}
		return fmt.Errorf("unit not healthy: %s", detail)
	}
	return nil
}

// probeDocker trusts the containers' own HEALTHCHECK, via the runtime's
// Observe.
func probeDocker(ctx context.Context, rt runtime.Runtime, t *runtime.Target) error {
	obs, err := rt.Observe(ctx, t)
	if err != nil {
		return err
	}
	if obs.State != runtime.StateRunning {
		detail := obs.Detail
		if detail == "" {
			detail = string(obs.State)
		}
		return fmt.Errorf("containers not healthy: %s", detail)
	}
	return nil
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return head
}
