package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	a.mu.Unlock()

	a.rebuildNotifiers()
	return nil
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
		logf("ignoring unreadable %s: %v", FleetConfigFile, err)
		return nil
	}
	a.fleet = &fc
	a.rebuildNotifiers()
	return nil
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
func (a *Agent) Rules() []alert.Rule {
	a.mu.RLock()
	fc := a.fleet
	services := make(map[string]*config.Service, len(a.services))
	for k, v := range a.services {
		services[k] = v
	}
	a.mu.RUnlock()

	var out []alert.Rule

	if fc != nil {
		for _, r := range fc.Alerts {
			if rule, ok := toRule("", r); ok {
				out = append(out, rule)
			}
		}
	}
	for _, name := range a.ServiceNames() {
		s := services[name]
		if s == nil {
			continue
		}
		for _, r := range s.Alerts {
			if rule, ok := toRule(name, r); ok {
				out = append(out, rule)
			}
		}
	}
	return out
}

func toRule(subject string, a config.Alert) (alert.Rule, bool) {
	cond, err := alert.Parse(a.When)
	if err != nil {
		logf("skipping alert %q: %v", a.When, err)
		return alert.Rule{}, false
	}
	return alert.Rule{
		Subject:  subject,
		Cond:     cond,
		For:      a.For.Duration(),
		Cooldown: a.Cooldown.Duration(),
		Notify:   a.Notify,
	}, true
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
	a.alerts.Evaluate(ctx, rules, a.readings(ctx))
}

// readings gathers the current value of every metric, keyed by subject. The
// host-wide reading is keyed by the empty string.
func (a *Agent) readings(ctx context.Context) map[string]alert.Reading {
	out := map[string]alert.Reading{}

	host := alert.Reading{}
	if d, err := a.diskUsage(); err == nil {
		host.DiskUsedPct = d.UsedPercent
	}
	out[""] = host

	for _, name := range a.ServiceNames() {
		s, ok := a.Service(name)
		if !ok {
			continue
		}
		r := alert.Reading{DiskUsedPct: host.DiskUsedPct}

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

	switch {
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

	// A rollback of a rollback is a forward move again, so only offer the undo
	// where it means what it says.
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
