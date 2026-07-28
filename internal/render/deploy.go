package render

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gandalfledev/pilot/internal/deploy"
	"github.com/gandalfledev/pilot/internal/runtime"
)

// Plan writes the human-readable form of a deploy plan.
func Plan(w io.Writer, p *deploy.Plan) {
	for _, hp := range p.Hosts {
		fmt.Fprintf(w, "  %s → %s\n", p.Service.Name, hp.Host)

		if hp.NoOp {
			fmt.Fprintf(w, "    unchanged  already running %s\n", hp.To)
			continue
		}

		from := hp.From
		if from == "" {
			from = "(none)"
		}
		fmt.Fprintf(w, "    release    %s → %s\n", from, hp.To)

		for _, c := range hp.Changes {
			if c.Field == "release" {
				continue
			}
			fmt.Fprintf(w, "    %-10s %s\n", c.Field, changeText(c))
		}

		fmt.Fprintf(w, "    route      %s\n", routeText(p, hp))

		strategy := "recreate"
		if p.Service.Rollout != nil && p.Service.Rollout.Strategy != "" {
			strategy = p.Service.Rollout.Strategy
		}
		fmt.Fprintf(w, "    strategy   %s%s\n", strategy, blipNote(p))
	}

	if p.Commit != "" {
		fmt.Fprintf(w, "\n  commit     %s\n", short(p.Commit, 12))
	}
}

func changeText(c deploy.Change) string {
	switch {
	case c.From != "" && c.To != "":
		return c.From + " → " + c.To
	case c.To != "":
		return "→ " + c.To
	}
	return c.From
}

func routeText(p *deploy.Plan, hp deploy.HostPlan) string {
	if p.Service.Expose == nil {
		return "none (service is not exposed)"
	}
	domains := strings.Join(p.Service.Expose.Domains, ", ")

	switch hp.Route {
	case deploy.RouteNone:
		return fmt.Sprintf("unchanged (%s)", domains)
	case deploy.RouteInstall:
		return fmt.Sprintf("install after health passes (%s)", domains)
	case deploy.RouteUpdate:
		return fmt.Sprintf("update on activate (%s)", domains)
	}
	return string(hp.Route)
}

// blipNote explains the user-visible cost of the strategy, since "recreate"
// alone doesn't tell an operator whether anyone will notice.
func blipNote(p *deploy.Plan) string {
	if p.Service.Expose != nil && !p.Service.Expose.IsStatic() {
		return " (brief restart, held by Caddy retry)"
	}
	if p.Service.Expose != nil {
		return " (no downtime — symlink swap)"
	}
	return ""
}

// Outcomes summarises what a deploy actually did.
func Outcomes(w io.Writer, outcomes []deploy.Outcome) {
	for _, o := range outcomes {
		switch {
		case o.Succeeded && o.Message == "unchanged":
			fmt.Fprintf(w, "  ✔ %s  unchanged\n", o.Host)
		case o.Succeeded:
			fmt.Fprintf(w, "  ✔ %s  now running %s\n", o.Host, o.Release)
		case o.RolledBack:
			fmt.Fprintf(w, "  ✖ %s  failed, rolled back to %s\n", o.Host, o.From)
			fmt.Fprintf(w, "      %s\n", firstLine(errText(o)))
		default:
			fmt.Fprintf(w, "  ✖ %s  failed\n", o.Host)
			fmt.Fprintf(w, "      %s\n", firstLine(errText(o)))
		}
	}
}

// Status writes the fleet overview table.
func Status(w io.Writer, rows []StatusRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "  no services")
		return
	}

	width := func(get func(StatusRow) string, header string) int {
		n := len(header)
		for _, r := range rows {
			if l := len(get(r)); l > n {
				n = l
			}
		}
		return n
	}

	sw := width(func(r StatusRow) string { return r.Service }, "SERVICE")
	hw := width(func(r StatusRow) string { return r.Host }, "HOST")
	stw := width(func(r StatusRow) string { return r.State }, "STATE")
	rw := width(func(r StatusRow) string { return r.Release }, "RELEASE")

	fmt.Fprintf(w, "  %-*s  %-*s  %-*s  %-*s  %s\n",
		sw, "SERVICE", hw, "HOST", stw, "STATE", rw, "RELEASE", "DETAIL")

	for _, r := range rows {
		detail := r.Detail
		if r.Drift != "" {
			detail = appendDetail("drift: "+r.Drift, detail)
		}
		fmt.Fprintf(w, "  %-*s  %-*s  %s%-*s%s  %-*s  %s\n",
			sw, r.Service, hw, r.Host,
			stateMark(r.State), stw, r.State, "",
			rw, dash(r.Release), detail)
	}
}

func appendDetail(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " · " + b
}

// StatusFooter reports what could not be determined and what is firing.
//
// Degradation is stated rather than hidden: a host with no agent is not the
// same as a host with nothing wrong, and the difference matters.
func StatusFooter(w io.Writer, degraded map[string]string, firing map[string][]string) {
	if len(firing) > 0 {
		fmt.Fprintln(w)
		for _, host := range sortedMapKeys(firing) {
			for _, rule := range firing[host] {
				fmt.Fprintf(w, "  ⚠ alert firing on %s: %s\n", host, rule)
			}
		}
	}

	if len(degraded) > 0 {
		fmt.Fprintln(w)
		for _, host := range sortedMapKeys(degraded) {
			fmt.Fprintf(w, "  – %s: no agent, so drift and alerts are unavailable\n", host)
			fmt.Fprintf(w, "      %s\n", firstLine(degraded[host]))
		}
		fmt.Fprintln(w, "    install one with `pilot bootstrap <host>`")
	}
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// StatusRow is one line of the fleet overview.
type StatusRow struct {
	Service string `json:"service"`
	Host    string `json:"host"`
	State   string `json:"state"`
	Release string `json:"release,omitempty"`

	// Drift names what diverged, empty when nothing has. Only an agent can
	// report it, so a host without one leaves this blank rather than claiming
	// everything is fine.
	Drift  string `json:"drift,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// InstanceRow is one container or process behind a service.
type InstanceRow struct {
	Service string `json:"service"`
	Host    string `json:"host"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
	Image   string `json:"image,omitempty"`
	Age     string `json:"age,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Instances writes the per-container table.
func Instances(w io.Writer, rows []InstanceRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "  nothing is running")
		return
	}

	width := func(get func(InstanceRow) string, header string) int {
		n := len(header)
		for _, r := range rows {
			if l := len(get(r)); l > n {
				n = l
			}
		}
		return n
	}

	sw := width(func(r InstanceRow) string { return r.Service }, "SERVICE")
	hw := width(func(r InstanceRow) string { return r.Host }, "HOST")
	nw := width(func(r InstanceRow) string { return r.Name }, "INSTANCE")
	stw := width(func(r InstanceRow) string { return r.State }, "STATE")
	aw := width(func(r InstanceRow) string { return r.Age }, "AGE")

	fmt.Fprintf(w, "  %-*s  %-*s  %-*s  %-*s  %-*s  %s\n",
		sw, "SERVICE", hw, "HOST", nw, "INSTANCE", stw, "STATE", aw, "AGE", "IMAGE")

	for _, r := range rows {
		state := r.State
		// Health is the more specific answer when the two disagree, and the
		// one an operator actually wants to see.
		if r.Health != "" && !strings.EqualFold(r.Health, "healthy") {
			state = r.Health
		}
		trailing := shortImage(r.Image)
		if trailing == "" {
			trailing = r.Detail
		}
		fmt.Fprintf(w, "  %-*s  %-*s  %-*s  %-*s  %-*s  %s\n",
			sw, r.Service, hw, r.Host, nw, r.Name,
			stw, state, aw, r.Age, trailing)
	}
}

// shortImage trims a digest to something a terminal can hold, keeping the
// repository, which is the part that identifies it.
func shortImage(ref string) string {
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ref
	}
	return repo + "@" + short(strings.TrimPrefix(digest, "sha256:"), 12)
}

func stateMark(state string) string {
	switch runtime.State(state) {
	case runtime.StateRunning:
		return "✔ "
	case runtime.StateDegraded:
		return "⚠ "
	case runtime.StateStopped, runtime.StateFailed:
		return "✖ "
	}
	return "? "
}

func dash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func errText(o deploy.Outcome) string {
	if o.Err != nil {
		return o.Err.Error()
	}
	return o.Message
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return head
}
