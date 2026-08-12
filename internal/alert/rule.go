// Package alert defines the condition language Pilot's alert rules are written
// in, and evaluates them.
//
// It is deliberately tiny — `<metric>` for a boolean, `<metric> <op> <number>`
// for a threshold — and deliberately shared: config validation and the agent
// parse rules with the same code, so a rule that validates on your laptop
// cannot fail to parse on the host at 3am.
package alert

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Metric is a thing a rule can be written about.
type Metric string

const (
	// Per-service metrics.
	ServiceDown     Metric = "service.down"
	ServiceDegraded Metric = "service.degraded"
	ServiceRestarts Metric = "service.restarts"
	DeployFailed    Metric = "deploy.failed"
	DriftDetected   Metric = "drift.detected"

	// Host-wide metrics.
	DiskUsedPct Metric = "host.disk.used_pct"
	DiskFreePct Metric = "host.disk.free_pct"
)

// Kind is whether a metric is a yes/no condition or a number to compare.
type Kind int

const (
	Boolean Kind = iota
	Numeric
)

// Scope is what a metric is measured against.
type Scope int

const (
	ScopeService Scope = iota
	ScopeHost
)

type metricInfo struct {
	kind  Kind
	scope Scope
	desc  string
}

var metrics = map[Metric]metricInfo{
	ServiceDown:     {Boolean, ScopeService, "the service is not running"},
	ServiceDegraded: {Boolean, ScopeService, "the service is running but not fully healthy"},
	ServiceRestarts: {Numeric, ScopeService, "how many times its containers have restarted"},
	DeployFailed:    {Boolean, ScopeService, "the most recent deploy failed or rolled back"},
	DriftDetected:   {Boolean, ScopeService, "live configuration no longer matches the manifest"},
	DiskUsedPct:     {Numeric, ScopeHost, "percentage of the Pilot volume in use"},
	DiskFreePct:     {Numeric, ScopeHost, "percentage of the Pilot volume still free"},
}

// Kind reports whether the metric is boolean or numeric.
func (m Metric) Kind() Kind { return metrics[m].kind }

// Scope reports whether the metric concerns one service or the whole host.
func (m Metric) Scope() Scope { return metrics[m].scope }

// Known reports whether the metric exists.
func (m Metric) Known() bool { _, ok := metrics[m]; return ok }

// KnownMetrics lists every metric, sorted, for error messages.
func KnownMetrics() []string {
	out := make([]string, 0, len(metrics))
	for m := range metrics {
		out = append(out, string(m))
	}
	sort.Strings(out)
	return out
}

// Op is a comparison.
type Op string

const (
	OpNone Op = ""
	OpGT   Op = ">"
	OpGTE  Op = ">="
	OpLT   Op = "<"
	OpLTE  Op = "<="
	OpEQ   Op = "=="
	OpNEQ  Op = "!="
)

var ops = []Op{OpGTE, OpLTE, OpNEQ, OpEQ, OpGT, OpLT} // longest first, so >= beats >

// Condition is a parsed rule expression.
type Condition struct {
	Metric Metric
	Op     Op
	Value  float64
	Raw    string
}

// Parse reads a rule expression.
//
// Boolean metrics stand alone (`service.down`); numeric ones need a comparison
// (`host.disk.free_pct < 10`). Mixing them up is the likely mistake, so each
// error says which kind the metric is.
func Parse(expr string) (Condition, error) {
	raw := strings.TrimSpace(expr)
	if raw == "" {
		return Condition{}, fmt.Errorf("empty condition")
	}

	name, op, valueStr := split(raw)

	m := Metric(name)
	if !m.Known() {
		return Condition{}, fmt.Errorf("unknown metric %q (known: %s)", name, strings.Join(KnownMetrics(), ", "))
	}

	c := Condition{Metric: m, Op: op, Raw: raw}

	switch m.Kind() {
	case Boolean:
		if op != OpNone {
			return Condition{}, fmt.Errorf("%s is a yes/no condition; write it on its own, without %q", name, op)
		}
	case Numeric:
		if op == OpNone {
			return Condition{}, fmt.Errorf("%s is a number; compare it, e.g. `%s > 3`", name, name)
		}
		v, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			return Condition{}, fmt.Errorf("%q is not a number in %q", valueStr, raw)
		}
		c.Value = v
	}
	return c, nil
}

// split separates an expression into metric, operator, and value.
func split(raw string) (name string, op Op, value string) {
	for _, candidate := range ops {
		if i := strings.Index(raw, string(candidate)); i >= 0 {
			return strings.TrimSpace(raw[:i]),
				candidate,
				strings.TrimSpace(raw[i+len(candidate):])
		}
	}
	return raw, OpNone, ""
}

// Describe renders the condition in prose, for alert messages.
func (c Condition) Describe() string {
	if c.Op == OpNone {
		return metrics[c.Metric].desc
	}
	return fmt.Sprintf("%s %s %s", metrics[c.Metric].desc, c.Op, trimFloat(c.Value))
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Reading is the observed value of every metric for one subject.
//
// Numbers and booleans travel together because a rule set mixes both, and
// gathering them once per evaluation is cheaper than asking per rule.
type Reading struct {
	ServiceDown     bool
	ServiceDegraded bool
	Restarts        int
	DeployFailed    bool
	DriftDetected   bool
	DiskUsedPct     int

	// Detail is the runtime's own one-line explanation of the state, e.g.
	// "last succeeded 60d ago, past the 48h freshness bound".
	//
	// It exists because the message that wakes somebody at 3am carried strictly
	// less information than the dashboard they were not looking at: the rule
	// name says `service.degraded`, and the generic description says "running
	// but not fully healthy", which for a scheduled job is not even true — it
	// is a timer, it is never running. The runtime already knows the real
	// reason; this carries it through.
	Detail string
}

// Eval reports whether the condition holds for a reading.
func (c Condition) Eval(r Reading) bool {
	switch c.Metric {
	case ServiceDown:
		return r.ServiceDown
	case ServiceDegraded:
		return r.ServiceDegraded
	case DeployFailed:
		return r.DeployFailed
	case DriftDetected:
		return r.DriftDetected
	case ServiceRestarts:
		return compare(float64(r.Restarts), c.Op, c.Value)
	case DiskUsedPct:
		return compare(float64(r.DiskUsedPct), c.Op, c.Value)
	case DiskFreePct:
		return compare(float64(100-r.DiskUsedPct), c.Op, c.Value)
	}
	return false
}

// Value returns the observed number a numeric condition was compared against,
// so an alert can say "12%" rather than only "below 15".
func (c Condition) Value_(r Reading) (float64, bool) {
	switch c.Metric {
	case ServiceRestarts:
		return float64(r.Restarts), true
	case DiskUsedPct:
		return float64(r.DiskUsedPct), true
	case DiskFreePct:
		return float64(100 - r.DiskUsedPct), true
	}
	return 0, false
}

func compare(got float64, op Op, want float64) bool {
	switch op {
	case OpGT:
		return got > want
	case OpGTE:
		return got >= want
	case OpLT:
		return got < want
	case OpLTE:
		return got <= want
	case OpEQ:
		return got == want
	case OpNEQ:
		return got != want
	}
	return false
}
