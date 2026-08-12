package doctor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// checkUnits reports systemd services whose unit is missing, or whose health
// gate is shorter than the time systemd will allow the unit to stop.
//
// Both problems are invisible in the configuration and only surface during a
// deploy — which is the deploy you were relying on to be safe.
func checkUnits(ctx context.Context, env *Env) []Finding {
	var out []Finding

	for _, name := range env.Fleet.ServiceNames() {
		s := env.Fleet.Services[name]
		if s.Runtime != config.RuntimeSystemd || s.Unit == nil || s.Unit.Name == "" {
			continue
		}

		for _, host := range s.Hosts {
			c := env.Client(host)
			if c == nil {
				continue // unreachable hosts are already reported once
			}
			out = append(out, inspectUnit(ctx, c, s, host)...)
		}
	}
	return out
}

// unitRunner is the slice of a client this check needs, so tests can supply a
// fake without standing up an ssh connection.
type unitRunner interface {
	Run(ctx context.Context, cmd string) (transport.Result, error)
}

func inspectUnit(ctx context.Context, c unitRunner, s *config.Service, host string) []Finding {
	u := s.Unit

	res, err := c.Run(ctx, transport.Join("systemctl", "show", u.Name,
		"-p", "LoadState,TimeoutStopUSec,UnitFileState"))
	if err != nil {
		return nil
	}
	props := parseProps(res.Stdout)

	// Pilot adopts units rather than writing them, so a missing one is a setup
	// step nobody did — and it fails at the restart, after the swap, with the
	// old release already gone.
	if props["LoadState"] == "not-found" {
		return []Finding{{
			Status: StatusFail, Scope: ScopeHost, Host: host,
			Title:  fmt.Sprintf("%s names unit %s, which does not exist on %s", s.Name, u.Name, host),
			Detail: "Pilot adopts an existing unit rather than writing one, so this deploy would stage a release, swap the symlink, and then fail at the restart with nothing running.",
			Hint:   fmt.Sprintf("install %s on %s, or correct `unit.name`", u.Name, host),
		}}
	}

	var out []Finding

	// A unit that exists but is disabled comes back stopped after a reboot,
	// while Pilot goes on reporting whatever it last observed.
	if st := props["UnitFileState"]; st == "disabled" && !u.IsOneshot() {
		out = append(out, Finding{
			Status: StatusWarn, Scope: ScopeHost, Host: host,
			Title:  fmt.Sprintf("%s is disabled on %s", u.Name, host),
			Detail: "It is running now, but it will not come back after a reboot.",
			Hint:   fmt.Sprintf("systemctl enable %s", u.Name),
		})
	}

	if f := checkStopBudget(s, host, props["TimeoutStopUSec"]); f != nil {
		out = append(out, *f)
	}
	return out
}

// checkStopBudget compares the health gate against systemd's stop timeout.
//
// This is the hopbox drain footgun, generalised. A unit with a long
// TimeoutStopSec has it for a reason: it needs that time to shut down cleanly —
// flushing, draining, snapshotting. If Pilot's health gate is shorter than the
// stop budget, then on a slow shutdown the gate expires while systemd is still
// legitimately stopping the old process, Pilot calls the deploy failed, and it
// rolls back a release that was fine.
//
// Worse, the rollback restarts the unit again, so a service that was mid-drain
// gets interrupted twice.
func checkStopBudget(s *config.Service, host, raw string) *Finding {
	stop, ok := parseUSec(raw)
	if !ok || stop <= 0 {
		return nil
	}

	// No health check means no gate to expire. That is a different problem and
	// the config validator already has opinions about it.
	if s.Health == nil || s.Health.Probes() == 0 {
		return nil
	}
	timeout := s.Health.Timeout.Duration()
	if timeout <= 0 {
		timeout = 60 * time.Second // deploy.Verify's default
	}
	if timeout > stop {
		return nil
	}

	return &Finding{
		Status: StatusFail, Scope: ScopeHost, Host: host,
		Title: fmt.Sprintf("%s allows %s to stop but only waits %s for health",
			s.Name, s.Unit.Name, timeout),
		Detail: fmt.Sprintf("%s has TimeoutStopSec=%s, which means systemd is willing to spend that "+
			"long letting it shut down cleanly — draining, flushing, or snapshotting. Pilot's health "+
			"gate expires first, so a slow but correct shutdown is read as a failed deploy and rolled "+
			"back, restarting the unit a second time while it was still busy.", s.Unit.Name, stop),
		Hint: fmt.Sprintf("set `health.timeout` above %s", stop),
	}
}

// parseUSec reads systemd's microsecond durations. It renders them as text
// ("1min 30s"), or as "infinity" when there is no limit at all — which cannot
// be exceeded and so is never a finding.
func parseUSec(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "infinity" {
		return 0, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Duration(n) * time.Microsecond, true
	}
	d, err := parseSystemdSpan(s)
	if err != nil {
		return 0, false
	}
	return d, true
}

// parseSystemdSpan reads the human form systemd prints: "1min 30s", "2min",
// "30s", "1h 5min".
func parseSystemdSpan(s string) (time.Duration, error) {
	units := map[string]time.Duration{
		"us": time.Microsecond, "ms": time.Millisecond,
		"s": time.Second, "sec": time.Second,
		"min": time.Minute, "m": time.Minute,
		"h": time.Hour, "d": 24 * time.Hour,
	}

	var total time.Duration
	var found bool
	for _, field := range strings.Fields(s) {
		i := strings.IndexFunc(field, func(r rune) bool { return r < '0' || r > '9' })
		if i <= 0 {
			continue
		}
		n, err := strconv.Atoi(field[:i])
		if err != nil {
			continue
		}
		mul, ok := units[field[i:]]
		if !ok {
			continue
		}
		total += time.Duration(n) * mul
		found = true
	}
	if !found {
		return 0, fmt.Errorf("no duration in %q", s)
	}
	return total, nil
}

// parseProps reads `systemctl show` output, splitting on the first `=` only.
func parseProps(out string) map[string]string {
	props := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			props[k] = v
		}
	}
	return props
}
