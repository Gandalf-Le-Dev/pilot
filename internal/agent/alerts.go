package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// AlertInterval is how often rules are evaluated. Frequent enough that a `for:`
// of a minute means roughly a minute, cheap because it reuses observations the
// agent is taking anyway.
const AlertInterval = 15 * time.Second

// FleetConfigFile caches the host-wide half of the configuration: notifiers and
// host-scoped rules, which have no service to hang off.
const FleetConfigFile = "_fleet.yaml"

// FleetConfig is what the CLI pushes so the agent can alert on its own.
//
// Aliased rather than defined here: the type lives in the config package so that
// SchemaFingerprint guards its field set, since the agent parses it strictly and
// a field it does not know is a rejected push.
type FleetConfig = config.FleetConfig

// PutFleetConfig caches the host-wide configuration.
func (a *Agent) PutFleetConfig(spec string) error {
	var fc FleetConfig
	if err := config.UnmarshalStrict([]byte(spec), &fc); err != nil {
		return err
	}

	if err := os.MkdirAll(a.cacheDir(), 0o755); err != nil {
		return err
	}
	path := filepath.Join(a.cacheDir(), FleetConfigFile)
	// 0600: this file holds notifier URLs, and a Discord or Slack webhook is a
	// credential — anyone holding it can post as you. It was world-readable.
	if err := release.WriteFileAtomic(path, []byte(spec), 0o600); err != nil {
		return err
	}

	a.mu.Lock()
	a.fleet = &fc
	a.fleetDigest = config.StampedDigest(spec)
	a.mu.Unlock()

	a.rebuildNotifiers()
	return nil
}

// FleetConfigDigest is the digest the CLI stamped on the cached host-wide
// config, or "" for none. `pilot doctor` compares it against the fleet as it
// stands, to find a host still working from an older config.
func (a *Agent) FleetConfigDigest() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.fleetDigest
}

func (a *Agent) loadFleetConfig() error {
	raw, err := os.ReadFile(filepath.Join(a.cacheDir(), FleetConfigFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var fc FleetConfig
	if err := config.UnmarshalStrict(raw, &fc); err != nil {
		a.log().Error("ignoring unreadable fleet config", "file", FleetConfigFile, "err", err)
		return nil
	}
	a.fleet = &fc
	a.fleetDigest = config.StampedDigest(string(raw))
	a.rebuildNotifiers()
	return nil
}

// CaddyBind returns the bind addresses pushed with the host-wide config, for
// routes the agent renders itself.
func (a *Agent) CaddyBind() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.fleet == nil {
		return nil
	}
	return a.fleet.CaddyBind
}

// ReportTarget returns where this host reports its public health, or nil when
// the fleet has no status page.
func (a *Agent) ReportTarget() *config.ReportTarget {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.fleet == nil {
		return nil
	}
	return a.fleet.Report
}

// rebuildNotifiers refreshes the delivery registry from the cached config.
func (a *Agent) rebuildNotifiers() {
	a.mu.RLock()
	fc := a.fleet
	a.mu.RUnlock()

	if fc == nil {
		return
	}
	var ns []alert.Notifier
	for name, n := range fc.Notifiers {
		ns = append(ns, alert.Notifier{
			Name:    name,
			Type:    alert.NotifierType(n.Type),
			URL:     n.Endpoint(),
			Command: n.Command,
		})
	}
	a.alerts.Sender = alert.NewRegistry(ns)
}

// Rules assembles every rule the agent should evaluate: host-wide ones from the
// cached fleet config, plus each service's own.
//
// Rules that fail to parse are skipped with a log line rather than aborting the
// sweep — one bad expression must not silence alerting for everything else.
// The line comes once per expression while it stays configured, not every
// sweep.
func (a *Agent) Rules() []alert.Rule {
	a.mu.RLock()
	fc := a.fleet
	services := make(map[string]*config.Service, len(a.services))
	for k, v := range a.services {
		services[k] = v
	}
	a.mu.RUnlock()

	var out []alert.Rule
	var bad []unparsed

	add := func(subject string, c config.Alert) {
		cond, err := alert.Parse(c.When)
		if err != nil {
			bad = append(bad, unparsed{subject, c.When, err})
			return
		}
		out = append(out, alert.Rule{
			Subject:  subject,
			Cond:     cond,
			For:      c.For.Duration(),
			Cooldown: c.Cooldown.Duration(),
			Notify:   c.Notify,
		})
	}

	if fc != nil {
		for _, r := range fc.Alerts {
			add("", r)
		}
	}
	for _, name := range a.ServiceNames() {
		s := services[name]
		if s == nil {
			continue
		}
		for _, r := range s.Alerts {
			add(name, r)
		}
	}
	a.warnSkipped(bad)
	return out
}

type unparsed struct {
	subject, when string
	err           error
}

// warnSkipped logs each unparseable rule not already logged. A rule that
// leaves the config is forgotten, so it is logged again if it comes back.
func (a *Agent) warnSkipped(bad []unparsed) {
	a.skippedMu.Lock()
	defer a.skippedMu.Unlock()

	now := make(map[string]bool, len(bad))
	for _, u := range bad {
		key := u.subject + "\x00" + u.when
		now[key] = true
		if !a.skipped[key] {
			a.log().Warn("skipping alert", "service", u.subject, "when", u.when, "err", u.err)
		}
	}
	a.skipped = now
}

// alertLoop evaluates rules on a schedule.
func (a *Agent) alertLoop(ctx context.Context) {
	tick := time.NewTicker(AlertInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		a.evaluateAlerts(ctx)
	}
}

// evaluateAlerts takes one reading per subject and runs the rules over it.
func (a *Agent) evaluateAlerts(ctx context.Context) {
	rules := a.Rules()
	if len(rules) == 0 {
		return
	}
	a.alerts.Evaluate(ctx, rules, a.readings(ctx, rules))
}

// readings gathers the current value of every metric, keyed by subject. The
// host-wide reading is keyed by the empty string. Memory is read only when a
// rule asks for it, and an unreadable disk or memory figure warns only when a
// rule reads it, which is when it costs an alert.
func (a *Agent) readings(ctx context.Context, rules []alert.Rule) map[string]alert.Reading {
	out := map[string]alert.Reading{}

	host := alert.Reading{}
	d, cause := a.readDisk(ctx)
	if d != nil {
		host.DiskUsedPct = &d.UsedPercent
	} else if cause != nil && readsDisk(rules) {
		a.diskWarnOnce.Do(func() {
			a.log().Warn("host disk unreadable, host.disk alert rules will not evaluate",
				append([]any{"path", a.Layout.Root}, cause...)...)
		})
	}
	if readsMemory(rules) {
		if pct, ok := a.memUsedPct(); ok {
			host.MemUsedPct = &pct
		}
	}
	out[""] = host

	for _, name := range a.ServiceNames() {
		s, ok := a.Service(name)
		if !ok {
			continue
		}
		r := alert.Reading{DiskUsedPct: host.DiskUsedPct, MemUsedPct: host.MemUsedPct}

		rt, err := RuntimeFor(s)
		if err != nil {
			continue
		}
		obs, err := rt.Observe(ctx, a.Target(s, ""))
		if err != nil {
			// An observation we could not take is not evidence of health, so
			// the subject is omitted and its rules hold their current state.
			continue
		}

		r.ServiceDown = obs.State != runtime.StateRunning
		r.ServiceDegraded = obs.State == runtime.StateDegraded
		r.Detail = obs.Detail
		for _, in := range obs.Instances {
			r.Restarts += in.Restarts
		}

		if d := a.Drift(name); d != nil {
			r.DriftDetected = d.config || d.route
		}
		if st, err := release.ReadState(a.Layout.Service(name), name); err == nil {
			if last := st.LastDeploy(); last != nil {
				r.DeployFailed = last.Outcome == release.OutcomeFailed ||
					last.Outcome == release.OutcomeRolledBack
			}
		}

		out[name] = r
	}
	return out
}

func readsDisk(rules []alert.Rule) bool {
	return slices.ContainsFunc(rules, func(r alert.Rule) bool {
		return r.Cond.Metric == alert.DiskUsedPct || r.Cond.Metric == alert.DiskFreePct
	})
}

func readsMemory(rules []alert.Rule) bool {
	return slices.ContainsFunc(rules, func(r alert.Rule) bool {
		return r.Cond.Metric == alert.MemUsedPct || r.Cond.Metric == alert.MemFreePct
	})
}

// memUsedPct is the share of the host's memory in use, counting reclaimable
// page cache as free. ok is false when it could not be read.
func (a *Agent) memUsedPct() (pct int, ok bool) {
	total, avail, err := a.readMeminfo()
	var cause []any
	switch {
	case err != nil:
		cause = []any{"err", err}
	case total == 0 || avail > total:
		cause = []any{"mem_total", total, "mem_available", avail}
	default:
		return int(math.Round(float64(total-avail) * 100 / float64(total))), true
	}
	a.memWarnOnce.Do(func() {
		a.log().Warn("host memory unreadable, host.mem alert rules will not evaluate",
			append([]any{"source", "/proc/meminfo"}, cause...)...)
	})
	return 0, false
}

// FiringAlerts reports which rules are currently firing.
func (a *Agent) FiringAlerts() []string { return a.alerts.Firing() }

// notifyDeployFinished announces a completed deploy or rollback.
//
// Unconditional on who caused it, and that is deliberate. Detecting whether a
// human or an agent ran the deploy would mean trusting what the caller says
// about itself, and the useful signal needs no such thing: a notification the
// user did not cause is the anomaly, whatever produced it.
//
// It carries the command that undoes the change. Holding someone responsible for
// what their tooling does is only coherent if they can act on it, and "a deploy
// happened" without "here is how to put it back" leaves them looking up syntax
// during the one minute it matters.
func (a *Agent) notifyDeployFinished(job proto.Job) {
	a.mu.RLock()
	fleet := a.fleet
	a.mu.RUnlock()

	if fleet == nil || !fleet.DeployNotificationsEnabled() || len(fleet.Notifiers) == 0 {
		return
	}

	msg := alert.Notification{
		Severity: alert.SevEvent,
		Host:     a.Host,
		Service:  job.Service,
		At:       job.FinishedAt,
	}

	started := job.Kind == proto.KindStart
	switch {
	case started && job.State == proto.JobFailed:
		msg.Summary = fmt.Sprintf("%s failed to start %s, the live release", job.Service, job.Release)
		msg.Detail = job.Error + "\nnothing was rolled back — check `pilot status`"
	case started:
		msg.Summary = fmt.Sprintf("%s was stopped and is running %s again", job.Service, job.Release)
	case job.State == proto.JobFailed && job.RolledBack:
		msg.Summary = fmt.Sprintf("%s failed to deploy %s and was rolled back", job.Service, job.Release)
		msg.Detail = job.Error
	case job.State == proto.JobFailed:
		msg.Summary = fmt.Sprintf("%s failed to deploy %s", job.Service, job.Release)
		msg.Detail = job.Error + "\nit may be left on the failed release — check `pilot status`"
	case job.Kind == proto.KindRollback:
		msg.Summary = fmt.Sprintf("%s rolled back to %s", job.Service, job.Release)
	default:
		msg.Summary = fmt.Sprintf("%s deployed %s", job.Service, job.Release)
	}

	// A rollback of a rollback is a forward move again, and a start changed no
	// release, so only offer the undo where it means what it says.
	if job.State == proto.JobSucceeded && job.Kind == proto.KindDeploy {
		msg.Detail = appendLine(msg.Detail, "undo with:  pilot rollback "+job.Service)
	}
	// Who ran it is deliberately absent. proto.Job does not carry it, and the
	// release history already records `By` for `pilot releases` to show — a
	// notification's job is "this happened, here is the undo", not an audit
	// trail that would need a wire change to carry.

	a.alerts.Notify(context.Background(), msg, notifierNames(fleet.Notifiers))
}

func appendLine(s, extra string) string {
	if s == "" {
		return extra
	}
	return s + "\n" + extra
}

// notifierNames returns every configured notifier, sorted so delivery order does
// not depend on map iteration.
func notifierNames(ns map[string]config.Notifier) []string {
	out := make([]string, 0, len(ns))
	for name := range ns {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
