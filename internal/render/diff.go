package render

import (
	"fmt"
	"io"
	"strings"
)

// DiffOp is what happened to a line.
type DiffOp int

const (
	DiffSame DiffOp = iota
	DiffRemoved
	DiffAdded
)

// DiffLine is one line of a rendered difference.
type DiffLine struct {
	Op   DiffOp
	Text string
}

// Diff compares two texts line by line.
//
// It trims the common prefix and suffix and reports the middle as a block
// replacement, rather than computing a minimal edit script. For the small,
// mostly-identical config files Pilot compares — a Caddy snippet against the
// one that shipped with a release — that produces output as readable as a real
// diff would, without the machinery.
func Diff(before, after string) []DiffLine {
	a := splitLines(before)
	b := splitLines(after)

	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}

	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}

	var out []DiffLine
	for _, l := range a[:prefix] {
		out = append(out, DiffLine{DiffSame, l})
	}
	for _, l := range a[prefix : len(a)-suffix] {
		out = append(out, DiffLine{DiffRemoved, l})
	}
	for _, l := range b[prefix : len(b)-suffix] {
		out = append(out, DiffLine{DiffAdded, l})
	}
	for _, l := range a[len(a)-suffix:] {
		out = append(out, DiffLine{DiffSame, l})
	}
	return out
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// WriteDiff renders a difference with context, eliding long runs of unchanged
// lines so the change is what you actually see.
func WriteDiff(w io.Writer, lines []DiffLine, context int) {
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if l.Op == DiffSame {
			continue
		}
		for j := max(0, i-context); j < min(len(lines), i+context+1); j++ {
			keep[j] = true
		}
	}

	elided := 0
	flush := func() {
		if elided > 0 {
			fmt.Fprintf(w, "        … %d unchanged line(s)\n", elided)
			elided = 0
		}
	}

	for i, l := range lines {
		if !keep[i] {
			elided++
			continue
		}
		flush()

		switch l.Op {
		case DiffRemoved:
			fmt.Fprintf(w, "      - %s\n", l.Text)
		case DiffAdded:
			fmt.Fprintf(w, "      + %s\n", l.Text)
		default:
			fmt.Fprintf(w, "        %s\n", l.Text)
		}
	}
	flush()
}

// DriftEntry is one service's divergence, ready to render.
//
// The json tags are not decoration. Without them Go emits the field names as
// written, so `pilot diff --json` was the one command answering in PascalCase
// with no `omitempty` — `"Service"` and `"RouteBefore": ""` where every other
// command says `"service"` and omits what is empty. A caller parsing Pilot's
// output had to special-case exactly one command, which is the sort of
// inconsistency that is discovered by something breaking.
type DriftEntry struct {
	Service string `json:"service"`
	Host    string `json:"host"`
	Release string `json:"release,omitempty"`

	ConfigDrift bool   `json:"config_drift,omitempty"`
	RouteDrift  bool   `json:"route_drift,omitempty"`
	Detail      string `json:"detail,omitempty"`

	// RouteBefore and RouteAfter are the deployed and installed routes, shown
	// as a diff when they differ.
	RouteBefore string `json:"route_before,omitempty"`
	RouteAfter  string `json:"route_after,omitempty"`

	// Unavailable explains why nothing could be determined.
	Unavailable string `json:"unavailable,omitempty"`
}

// Drifted reports whether anything actually diverged.
func (d DriftEntry) Drifted() bool { return d.ConfigDrift || d.RouteDrift }

// Diff writes the drift report.
func WriteDrift(w io.Writer, entries []DriftEntry) {
	var drifted int

	for _, e := range entries {
		fmt.Fprintf(w, "  %s on %s\n", e.Service, e.Host)

		switch {
		case e.Unavailable != "":
			fmt.Fprintf(w, "    ? %s\n", e.Unavailable)
			continue
		case !e.Drifted():
			fmt.Fprintf(w, "    ✔ matches release %s\n", dash(e.Release))
			continue
		}

		drifted++

		if e.ConfigDrift {
			fmt.Fprintf(w, "    ✖ configuration differs from release %s\n", dash(e.Release))
			// Only a digest is compared, so there is nothing line-by-line to
			// show. Saying so beats implying we looked and found nothing.
			fmt.Fprintln(w, "      the live compose config or environment no longer matches what was deployed")
		}

		if e.RouteDrift {
			fmt.Fprintln(w, "    ✖ installed route differs from the one deployed with this release")
			if e.RouteBefore != "" || e.RouteAfter != "" {
				fmt.Fprintln(w, "      (- deployed, + installed)")
				WriteDiff(w, Diff(e.RouteBefore, e.RouteAfter), 2)
			}
		}
	}

	fmt.Fprintln(w)
	if drifted == 0 {
		fmt.Fprintln(w, "  nothing has drifted")
		return
	}
	fmt.Fprintf(w, "  %s drifted — `pilot deploy` restores the declared state\n",
		countNoun(drifted, "service", "services"))
}
