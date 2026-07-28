// Package render turns Pilot's results into terminal output.
//
// Rendering is kept separate from the checks and the deploy pipeline so that
// --json is never an afterthought: every command produces data first and text
// second.
package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/doctor"
)

// Doctor writes a grouped report: config first, then one block per host in
// fleet order, then the edge.
func Doctor(w io.Writer, r *doctor.Report, hostOrder []string) {
	groups := r.Grouped(hostOrder)

	for i, g := range groups {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "  %s\n", groupTitle(g))
		for _, f := range g.Findings {
			writeFinding(w, f)
		}
	}

	if len(groups) > 0 {
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  %s\n", r.Summary())

	if n := len(r.Fixable()); n > 0 {
		fmt.Fprintf(w, "  %s can be repaired automatically — run `pilot doctor --fix`\n",
			countNoun(n, "finding", "findings"))
	}
}

func groupTitle(g doctor.Group) string {
	if g.Host != "" {
		return g.Host
	}
	return string(g.Scope)
}

func writeFinding(w io.Writer, f doctor.Finding) {
	line := fmt.Sprintf("    %s %s", f.Status.Symbol(), f.Title)
	if f.Detail != "" {
		line += "  " + f.Detail
	}
	if f.Fixable() {
		line += "     → --fix"
	}
	fmt.Fprintln(w, line)

	// Hints go on their own line, indented under the finding, so the common
	// case stays scannable and the remedy is still right there when needed.
	if f.Hint != "" {
		for _, l := range wrap(f.Hint, 72) {
			fmt.Fprintf(w, "        %s\n", l)
		}
	}
}

// countNoun renders "1 finding" / "3 findings".
func countNoun(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// wrap breaks text at word boundaries, so a long hint doesn't produce one
// unreadable line in a narrow terminal.
func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}

	var lines []string
	cur := words[0]
	for _, word := range words[1:] {
		if len(cur)+1+len(word) > width {
			lines = append(lines, cur)
			cur = word
			continue
		}
		cur += " " + word
	}
	return append(lines, cur)
}
