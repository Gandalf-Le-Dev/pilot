package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
)

// probeTimeout bounds a single health check attempt, so one hung probe cannot
// consume the whole verification window.
const probeTimeout = 10 * time.Second

// Probe runs one health check for a service.
//
// Running inside the agent means these are real in-process checks rather than
// shelled-out curl invocations: fewer moving parts, no dependency on which
// utilities happen to be installed, and honest error messages.
//
// The probe *implementations* differ from deploy.Verify's deliberately, for
// that reason. The dispatch below does not: it must handle exactly the same set
// of kinds, or a health check silently means something different depending on
// whether an agent happened to be present. Adding `systemd` to config and only
// to the CLI's copy produced precisely that — a deploy that planned, staged and
// activated, then failed verification on the host alone. The test alongside
// this file asserts every kind reaches a real branch here.
func Probe(ctx context.Context, rt runtime.Runtime, t *runtime.Target) error {
	h := t.Service.Health
	if h == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	switch {
	case h.HTTP != nil:
		return probeHTTP(ctx, h.HTTP)
	case h.TCP != nil:
		return probeTCP(ctx, h.TCP)
	case h.Exec != nil:
		return probeExec(ctx, h.Exec)
	case h.Docker, h.Systemd:
		// Both defer to the runtime's own Observe. For systemd that is what
		// makes the probe mean the right thing for a oneshot: `systemctl
		// is-active` would report every scheduled job as unhealthy forever,
		// since a oneshot is inactive by design the moment it finishes.
		return probeRuntime(ctx, rt, t)
	}
	return nil
}

func probeHTTP(ctx context.Context, p *config.HTTPProbe) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return fmt.Errorf("bad health check URL %q: %w", p.URL, err)
	}

	// No redirect following: a health check that 302s to a login page is not
	// healthy, and silently following it would hide that.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", p.URL, err)
	}
	defer resp.Body.Close()

	want := p.Expect
	if want == 0 {
		want = 200
	}
	if resp.StatusCode != want {
		return fmt.Errorf("GET %s returned %d, want %d", p.URL, resp.StatusCode, want)
	}
	return nil
}

func probeTCP(ctx context.Context, p *config.TCPProbe) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", p.Addr)
	if err != nil {
		return fmt.Errorf("cannot connect to %s: %w", p.Addr, err)
	}
	return conn.Close()
}

// probeExec runs a command on the host.
//
// On the host, not inside a container: guessing which container of a
// multi-service stack to enter would be wrong as often as right. To probe
// inside one, name it — `docker compose -p api exec -T db pg_isready` says
// exactly what it does.
func probeExec(ctx context.Context, p *config.ExecProbe) error {
	if len(p.Cmd) == 0 {
		return fmt.Errorf("exec health check has no command")
	}

	cmd := exec.CommandContext(ctx, p.Cmd[0], p.Cmd[1:]...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return fmt.Errorf("%s failed: %w", p.Cmd[0], err)
	}
	return fmt.Errorf("%s failed: %s", p.Cmd[0], firstLine(detail))
}

// probeRuntime trusts the workload's own notion of health, via Observe.
func probeRuntime(ctx context.Context, rt runtime.Runtime, t *runtime.Target) error {
	obs, err := rt.Observe(ctx, t)
	if err != nil {
		return err
	}
	if obs.State != runtime.StateRunning {
		detail := obs.Detail
		if detail == "" {
			detail = string(obs.State)
		}
		return fmt.Errorf("not healthy: %s", detail)
	}
	return nil
}
