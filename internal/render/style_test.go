package render

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/doctor"
)

// ansi matches any escape sequence, colour or otherwise.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func sampleRows() []StatusRow {
	return []StatusRow{
		{Service: "api", Host: "web-1", State: "running", Release: "0042-9f3ac1b"},
		{Service: "db", Host: "web-1", State: "stopped", Release: "0007-abc1234"},
		{Service: "site", Host: "web-2", State: "unknown", Drift: "config", Detail: "2 instance(s)"},
	}
}

func sampleReport() *doctor.Report {
	return &doctor.Report{Findings: []doctor.Finding{
		{Status: doctor.StatusOK, Scope: doctor.ScopeConfig, Title: "fleet.yaml valid"},
		{Status: doctor.StatusWarn, Scope: doctor.ScopeHost, Host: "web-1", Title: "disk 91% used", Detail: "cleanup soon"},
		{Status: doctor.StatusFail, Scope: doctor.ScopeHost, Host: "web-1", Title: "unreachable"},
	}}
}

// TestNoEscapesWhenNotATerminal is the guard this styling needs.
//
// Pilot's output is read by three things and only one is a person: a terminal,
// a script parsing columns, and an AI agent with the skill installed. Escape
// codes improve the first and corrupt the other two — an agent gets its context
// filled with noise it cannot use, and a pipeline's parsing breaks in a way that
// surfaces months later in somebody's cron job.
//
// A bytes.Buffer is not a terminal, which is exactly the case a pipe exercises.
func TestNoEscapesWhenNotATerminal(t *testing.T) {
	cases := map[string]func(*bytes.Buffer){
		"Status": func(b *bytes.Buffer) { Status(b, sampleRows()) },
		"StatusFooter": func(b *bytes.Buffer) {
			StatusFooter(b, map[string]string{"web-2": "no agent is listening"}, map[string][]string{"web-1": {"disk"}})
		},
		"Doctor": func(b *bytes.Buffer) { Doctor(b, sampleReport(), []string{"web-1", "web-2"}) },
	}

	for name, render := range cases {
		var b bytes.Buffer
		render(&b)

		if got := ansi.FindString(b.String()); got != "" {
			t.Errorf("%s emitted escape sequence %q to a non-terminal:\n%s",
				name, got, b.String())
		}
	}
}

// TestStylingAppliesWhenEnabled proves the styling is real rather than dead
// code that the guard above passes trivially.
func TestStylingAppliesWhenEnabled(t *testing.T) {
	defer ColourOverride(true)()

	var b bytes.Buffer
	Status(&b, sampleRows())

	if !ansi.MatchString(b.String()) {
		t.Fatal("no escape sequences with colour forced; the styling is not wired up")
	}
}

// TestStylingDoesNotChangeTheText — colour is decoration. Strip it and the
// output must be byte-identical to the unstyled form, or the two renderings
// have diverged and one of them is wrong.
func TestStylingDoesNotChangeTheText(t *testing.T) {
	var plain bytes.Buffer
	Status(&plain, sampleRows())

	restore := ColourOverride(true)
	var styled bytes.Buffer
	Status(&styled, sampleRows())
	restore()

	if got := ansi.ReplaceAllString(styled.String(), ""); got != plain.String() {
		t.Errorf("styling changed the text itself:\n plain: %q\nstyled: %q", plain.String(), got)
	}
}

// TestColumnsStayAlignedWhenColoured is the specific trap in styling a table:
// escape sequences have no display width, so padding computed after colouring
// misaligns every row that happens to be coloured — and only those rows, which
// makes it look like a data problem rather than a formatting one.
func TestColumnsStayAlignedWhenColoured(t *testing.T) {
	restore := ColourOverride(true)
	var b bytes.Buffer
	Status(&b, sampleRows())
	restore()

	var widths []int
	for _, line := range strings.Split(ansi.ReplaceAllString(b.String(), ""), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The RELEASE column starts at a fixed offset; find it via the gap
		// before it rather than trusting any single row.
		widths = append(widths, strings.Index(line, "  0")+len(line)*0)
	}
	// Every data row must place its release column identically.
	seen := map[int]bool{}
	for _, w := range widths {
		if w > 0 {
			seen[w] = true
		}
	}
	if len(seen) > 1 {
		t.Errorf("release column starts at differing offsets %v; padding was applied after colouring", seen)
	}
}
