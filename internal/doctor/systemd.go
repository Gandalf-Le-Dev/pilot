package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
			out = append(out, compareReference(ctx, c, env.Fleet.Root, s, host)...)
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

// compareReference checks the host's effective unit against the definition
// recorded in the fleet repo.
//
// This is the other half of Pilot adopting units rather than writing them.
// Pilot deliberately never installs the file, which leaves the unit living only
// on the server unless something records it — and Fingerprint is no help there,
// because it only notices a unit changing after a deploy captured a baseline.
// A unit that was wrong from the first deploy is drift from nothing.
func compareReference(ctx context.Context, c unitRunner, root string, s *config.Service, host string) []Finding {
	u := s.Unit
	if u.Reference == "" {
		return nil
	}

	want, err := readReference(root, s)
	if err != nil {
		// The path points at nothing. That is a configuration mistake rather
		// than a host problem, and silently skipping it would mean the check
		// reports clean precisely when it is not running.
		return []Finding{{
			Status: StatusFail, Scope: ScopeConfig,
			Title:  fmt.Sprintf("%s references a unit file that does not exist: %s", s.Name, u.Reference),
			Detail: "`unit.reference` is read from the service's own directory. Nothing was compared.",
			Hint:   fmt.Sprintf("check the path, or copy the live unit with: systemctl cat %s", u.Name),
		}}
	}

	res, err := c.Run(ctx, transport.Join("systemctl", "cat", u.Name))
	if err != nil || res.ExitCode != 0 {
		return nil // a missing unit is already reported by inspectUnit
	}

	added, removed := diffUnits(want, res.Stdout)
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}

	return []Finding{{
		Status: StatusWarn, Scope: ScopeHost, Host: host,
		Title: fmt.Sprintf("%s on %s does not match %s", u.Name, host, u.Reference),
		Detail: "The unit running on the host differs from the definition recorded in the fleet. " +
			"Pilot does not install units, so the two drift apart silently.\n" +
			unitDiff(added, removed),
		Hint: fmt.Sprintf("apply the recorded unit, or update %s to match the host", u.Reference),
	}}
}

func readReference(root string, s *config.Service) ([]byte, error) {
	var dir string
	if s.Source != nil {
		dir = s.Source.Path
	}
	if dir == "" {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(root, dir, s.Unit.Reference))
}

// diffUnits compares a recorded unit against `systemctl cat` output.
//
// The comparison is line-set based rather than positional: reordering keys
// inside a systemd section changes nothing about behaviour, and a check that
// cried drift over it would be turned off within a week.
func diffUnits(want []byte, got string) (added, removed []string) {
	w := unitLines(string(want))
	g := unitLines(got)

	inW := map[string]int{}
	for _, l := range w {
		inW[l]++
	}
	inG := map[string]int{}
	for _, l := range g {
		inG[l]++
	}

	for _, l := range g {
		if inW[l] == 0 {
			added = append(added, l)
			inW[l]--
		}
	}
	for _, l := range w {
		if inG[l] == 0 {
			removed = append(removed, l)
			inG[l]--
		}
	}
	return dedupe(added), dedupe(removed)
}

// unitLines normalises a unit for comparison.
//
// `systemctl cat` prefixes each source file with a `# /path/to/unit` line and
// concatenates drop-ins after it. Those path lines are systemd's framing, not
// the operator's content, and comparing them would report drift on every host
// whose unit lives somewhere slightly different. Everything else is kept —
// including comments, because a comment explaining why TimeoutStopSec is 90s is
// exactly the kind of thing worth noticing has been deleted.
func unitLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimRight(line, " \t\r")
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "# /"), strings.HasPrefix(line, "# ‣ /"):
			continue // systemctl's file-path framing
		}
		out = append(out, line)
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// unitDiff renders the difference, bounded so one reordered file cannot fill a
// terminal with a wall of text nobody reads.
func unitDiff(added, removed []string) string {
	const max = 6
	var b strings.Builder
	for i, l := range removed {
		if i == max {
			fmt.Fprintf(&b, "      … and %d more only in the recorded unit\n", len(removed)-max)
			break
		}
		fmt.Fprintf(&b, "      - %s\n", l)
	}
	for i, l := range added {
		if i == max {
			fmt.Fprintf(&b, "      … and %d more only on the host\n", len(added)-max)
			break
		}
		fmt.Fprintf(&b, "      + %s\n", l)
	}
	return strings.TrimRight(b.String(), "\n")
}
