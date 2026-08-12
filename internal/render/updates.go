package render

import (
	"fmt"
	"io"

	"github.com/Gandalf-Le-Dev/pilot/internal/registry"
	"github.com/Gandalf-Le-Dev/pilot/internal/updates"
)

// Updates writes the image-update table.
//
// Ordered so the actionable rows come first and the rest is available but out
// of the way. A checker that lists everything with equal weight teaches you to
// skim it, and then the one release that mattered is skimmed too.
func Updates(w io.Writer, rows []updates.Result) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "  no images to check")
		return
	}

	st := newStyler(w)

	var outdated, current, skipped []updates.Result
	for _, r := range rows {
		switch {
		case r.Err != "" || r.Track:
			skipped = append(skipped, r)
		case r.Outdated():
			outdated = append(outdated, r)
		default:
			current = append(current, r)
		}
	}

	width := func(rows []updates.Result, get func(updates.Result) string, min int) int {
		n := min
		for _, r := range rows {
			if l := len(get(r)); l > n {
				n = l
			}
		}
		return n
	}
	shown := append(append([]updates.Result{}, outdated...), current...)
	sw := width(shown, func(r updates.Result) string { return r.Service }, len("SERVICE"))
	iw := width(shown, func(r updates.Result) string { return r.Image }, len("IMAGE"))
	cw := width(shown, func(r updates.Result) string { return r.Current }, len("CURRENT"))
	lw := width(shown, func(r updates.Result) string { return r.Latest }, len("LATEST"))

	if len(shown) > 0 {
		fmt.Fprintf(w, "  %s\n", st.header(fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %s",
			sw, "SERVICE", iw, "IMAGE", cw, "CURRENT", lw, "LATEST", "")))
	}

	for _, r := range outdated {
		fmt.Fprintf(w, "  %-*s  %-*s  %-*s  %-*s  %s\n",
			sw, r.Service, iw, r.Image, cw, r.Current, lw, r.Latest, stepLabel(st, r.Step))
	}
	for _, r := range current {
		fmt.Fprintf(w, "  %s\n", st.muted(fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %s",
			sw, r.Service, iw, r.Image, cw, r.Current, lw, "", "up to date")))
	}

	if len(skipped) > 0 {
		fmt.Fprintln(w)
		for _, r := range skipped {
			reason := r.Err
			if r.Track {
				// Not a failure — a deliberate choice being respected.
				reason = "moving tag, not compared"
			}
			fmt.Fprintf(w, "  %s\n", st.muted(fmt.Sprintf("– %s:%s (%s): %s",
				r.Image, r.Current, r.Service, reason)))
		}
	}

	fmt.Fprintln(w)
	switch n := len(outdated); {
	case n == 0:
		fmt.Fprintln(w, "  everything pinned is up to date")
	case n == 1:
		fmt.Fprintln(w, "  1 update available")
	default:
		fmt.Fprintf(w, "  %d updates available\n", n)
	}
}

// stepLabel colours by how much is changing. Major gets attention because it is
// the one that will not be a drop-in.
func stepLabel(st styler, step registry.Step) string {
	switch step {
	case registry.StepPatch:
		return st.ok_("patch")
	case registry.StepMinor:
		return st.warn("minor")
	case registry.StepMajor:
		return st.fail("major")
	}
	return ""
}
