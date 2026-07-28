// Package doctor answers one question: is this setup sound?
//
// Checks are values, not hard-coded procedure, because three entry points share
// them. `pilot doctor` runs all of them; `pilot bootstrap` is doctor plus
// installation; `pilot deploy` runs the subset relevant to what it is about to
// touch, so a broken setup surfaces before bytes move rather than halfway
// through a rollout.
package doctor

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/ssh"
)

// Status is the verdict of one finding.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
	StatusSkipped
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	case StatusSkipped:
		return "skipped"
	}
	return "unknown"
}

// MarshalJSON emits the status name rather than its ordinal, so consumers of
// --json don't have to know the enum's internal numbering.
func (s Status) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// UnmarshalJSON accepts the name form written by MarshalJSON.
func (s *Status) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(b), `"`) {
	case "ok":
		*s = StatusOK
	case "warn":
		*s = StatusWarn
	case "fail":
		*s = StatusFail
	case "skipped":
		*s = StatusSkipped
	default:
		return fmt.Errorf("unknown status %s", b)
	}
	return nil
}

// Symbol is the glyph shown in the report.
func (s Status) Symbol() string {
	switch s {
	case StatusOK:
		return "✔"
	case StatusWarn:
		return "⚠"
	case StatusFail:
		return "✖"
	case StatusSkipped:
		return "–"
	}
	return "?"
}

// Scope groups findings in the report.
type Scope string

const (
	ScopeConfig Scope = "config"
	ScopeHost   Scope = "host"
	ScopeEdge   Scope = "edge"
)

// Finding is one observation about the setup.
type Finding struct {
	Status Status `json:"status"`
	Scope  Scope  `json:"scope"`

	// Host is the fleet host this concerns, empty for fleet-wide findings.
	Host string `json:"host,omitempty"`

	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`

	// Fix repairs the problem, when it can be repaired unambiguously and
	// without touching a running service. Nil otherwise. The check that found
	// the problem is the thing that knows how to undo it, so the remedy travels
	// with the finding rather than living in a lookup table somewhere else.
	Fix     func(context.Context) error `json:"-"`
	FixDesc string                      `json:"fix,omitempty"`
}

// Fixable reports whether --fix can repair this finding.
func (f Finding) Fixable() bool { return f.Fix != nil }

// Env is what checks are given to work with.
type Env struct {
	Fleet *config.Fleet
	Diags config.Diagnostics

	// Clients holds a connection per reachable host. A host missing from this
	// map is unreachable, and host-scoped checks skip it rather than reporting
	// a cascade of failures that all mean the same thing.
	Clients map[string]*ssh.Client

	// Offline suppresses every check that needs the network, so config
	// validation alone can run in CI.
	Offline bool
}

// Client returns the connection for a host, or nil when unreachable.
func (e *Env) Client(host string) *ssh.Client { return e.Clients[host] }

// ServicesOn returns the services placed on a host, sorted by name.
func (e *Env) ServicesOn(host string) []*config.Service {
	var out []*config.Service
	for _, name := range e.Fleet.ServiceNames() {
		s := e.Fleet.Services[name]
		if slices.Contains(s.Hosts, host) {
			out = append(out, s)
		}
	}
	return out
}

// Check is one named inspection.
type Check struct {
	Name  string
	Scope Scope

	// NeedsNetwork marks checks skipped under --offline.
	NeedsNetwork bool

	Run func(ctx context.Context, env *Env) []Finding
}

// Report is the outcome of a doctor run.
type Report struct {
	Findings []Finding `json:"findings"`
}

// Run executes checks in order and collects their findings.
func Run(ctx context.Context, env *Env, checks []Check) *Report {
	r := &Report{}
	for _, c := range checks {
		if c.NeedsNetwork && env.Offline {
			continue
		}
		if err := ctx.Err(); err != nil {
			break
		}
		r.Findings = append(r.Findings, c.Run(ctx, env)...)
	}
	return r
}

// Count returns how many findings carry a status.
func (r *Report) Count(s Status) int {
	n := 0
	for _, f := range r.Findings {
		if f.Status == s {
			n++
		}
	}
	return n
}

func (r *Report) Errors() int   { return r.Count(StatusFail) }
func (r *Report) Warnings() int { return r.Count(StatusWarn) }

// Fixable returns the findings --fix can repair, in report order.
func (r *Report) Fixable() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Fixable() {
			out = append(out, f)
		}
	}
	return out
}

// ExitCode maps the report onto a process exit status, so `pilot doctor` drops
// straight into cron or CI: 0 clean, 1 errors present, 2 warnings only.
func (r *Report) ExitCode() int {
	switch {
	case r.Errors() > 0:
		return 1
	case r.Warnings() > 0:
		return 2
	}
	return 0
}

// Summary is the one-line tally shown at the end of a run.
func (r *Report) Summary() string {
	e, w := r.Errors(), r.Warnings()
	if e == 0 && w == 0 {
		return "all checks passed"
	}
	var parts []string
	if e > 0 {
		parts = append(parts, plural(e, "error"))
	}
	if w > 0 {
		parts = append(parts, plural(w, "warning"))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strings.TrimSpace(itoa(n) + " " + word + "s")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Groups organises findings for display: config first, then one group per host
// in fleet order, then edge.
type Group struct {
	Scope    Scope
	Host     string
	Findings []Finding
}

// Grouped arranges the report for rendering.
func (r *Report) Grouped(hostOrder []string) []Group {
	rank := map[string]int{}
	for i, h := range hostOrder {
		rank[h] = i
	}

	byKey := map[string]*Group{}
	var keys []string
	for _, f := range r.Findings {
		key := string(f.Scope) + "\x00" + f.Host
		g, ok := byKey[key]
		if !ok {
			g = &Group{Scope: f.Scope, Host: f.Host}
			byKey[key] = g
			keys = append(keys, key)
		}
		g.Findings = append(g.Findings, f)
	}

	scopeRank := map[Scope]int{ScopeConfig: 0, ScopeHost: 1, ScopeEdge: 2}
	sort.SliceStable(keys, func(i, j int) bool {
		gi, gj := byKey[keys[i]], byKey[keys[j]]
		if gi.Scope != gj.Scope {
			return scopeRank[gi.Scope] < scopeRank[gj.Scope]
		}
		ri, oki := rank[gi.Host]
		rj, okj := rank[gj.Host]
		if oki && okj {
			return ri < rj
		}
		return gi.Host < gj.Host
	})

	out := make([]Group, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	return out
}
