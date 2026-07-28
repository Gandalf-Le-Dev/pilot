package agent

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
)

// AlertInterval is how often rules are evaluated. Frequent enough that a `for:`
// of a minute means roughly a minute, cheap because it reuses observations the
// agent is taking anyway.
const AlertInterval = 15 * time.Second

// FleetConfigFile caches the host-wide half of the configuration: notifiers and
// host-scoped rules, which have no service to hang off.
const FleetConfigFile = "_fleet.yaml"

// FleetConfig is what the CLI pushes so the agent can alert on its own.
type FleetConfig struct {
	Notifiers map[string]config.Notifier `yaml:"notifiers"`
	Alerts    []config.Alert             `yaml:"alerts"`
}

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
	if err := release.WriteFileAtomic(path, []byte(spec), 0o644); err != nil {
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
