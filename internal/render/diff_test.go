package render

import (
	"strings"
	"testing"
)

func ops(lines []DiffLine) string {
	var b strings.Builder
	for _, l := range lines {
		switch l.Op {
		case DiffRemoved:
			b.WriteByte('-')
		case DiffAdded:
			b.WriteByte('+')
		default:
			b.WriteByte('=')
		}
	}
	return b.String()
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name          string
		before, after string
		want          string
	}{
		{"identical", "a\nb\nc\n", "a\nb\nc\n", "==="},
		{"one line changed in the middle", "a\nb\nc\n", "a\nX\nc\n", "=-+="},
		{"line added", "a\nc\n", "a\nb\nc\n", "=+="},
		{"line removed", "a\nb\nc\n", "a\nc\n", "=-="},
		{"everything replaced", "a\n", "z\n", "-+"},
		{"empty before", "", "a\n", "+"},
		{"empty after", "a\n", "", "-"},
		{"both empty", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ops(Diff(tc.before, tc.after)); got != tc.want {
				t.Errorf("Diff ops = %q, want %q", got, tc.want)
			}
		})
	}
}

// Trailing newlines are a formatting detail of the file, not a change worth
// reporting.
func TestDiffIgnoresTrailingNewline(t *testing.T) {
	if got := ops(Diff("a\nb", "a\nb\n")); got != "==" {
		t.Errorf("ops = %q, want no differences", got)
	}
}

func TestWriteDiffMarksChanges(t *testing.T) {
	var b strings.Builder
	WriteDiff(&b, Diff(
		"blog.example.com {\n\tencode gzip zstd\n\tfile_server\n}\n",
		"blog.example.com {\n\tencode gzip zstd\n\tbasicauth * { admin HASH }\n\tfile_server\n}\n",
	), 2)

	got := b.String()
	if !strings.Contains(got, "+ \tbasicauth") {
		t.Errorf("added line not marked:\n%s", got)
	}
	if strings.Contains(got, "- ") {
		t.Errorf("nothing was removed, so nothing should be marked as such:\n%s", got)
	}
}

// A change buried in a long file should be what you actually see.
func TestWriteDiffElidesUnchangedRuns(t *testing.T) {
	before := strings.Repeat("same\n", 40) + "old\n" + strings.Repeat("same\n", 40)
	after := strings.Repeat("same\n", 40) + "new\n" + strings.Repeat("same\n", 40)

	var b strings.Builder
	WriteDiff(&b, Diff(before, after), 2)
	got := b.String()

	if !strings.Contains(got, "unchanged line(s)") {
		t.Errorf("long unchanged runs should be elided:\n%s", got)
	}
	if !strings.Contains(got, "- old") || !strings.Contains(got, "+ new") {
		t.Errorf("the actual change should be shown:\n%s", got)
	}
	if n := strings.Count(got, "same"); n > 10 {
		t.Errorf("%d unchanged lines survived elision; the change should stand out", n)
	}
}

func TestWriteDriftReportsCleanServices(t *testing.T) {
	var b strings.Builder
	WriteDrift(&b, []DriftEntry{
		{Service: "api", Host: "web-1", Release: "0042-9f3ac1b"},
	})

	got := b.String()
	if !strings.Contains(got, "matches release 0042-9f3ac1b") {
		t.Errorf("a clean service should say so:\n%s", got)
	}
	if !strings.Contains(got, "nothing has drifted") {
		t.Errorf("summary missing:\n%s", got)
	}
}

func TestWriteDriftShowsRouteDifference(t *testing.T) {
	var b strings.Builder
	WriteDrift(&b, []DriftEntry{{
		Service: "blog", Host: "web-1", Release: "0001-aaaaaaa",
		RouteDrift:  true,
		RouteBefore: "blog.example.com {\n\tfile_server\n}\n",
		RouteAfter:  "blog.example.com {\n\tbasicauth * { admin HASH }\n\tfile_server\n}\n",
	}})

	got := b.String()
	for _, want := range []string{
		"installed route differs",
		"(- deployed, + installed)",
		"+ \tbasicauth",
		"1 service drifted",
		"pilot deploy",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Only a digest is compared for config drift, so the report must not imply it
// looked line by line and found nothing.
func TestWriteDriftExplainsConfigDriftWithoutAFakeDiff(t *testing.T) {
	var b strings.Builder
	WriteDrift(&b, []DriftEntry{{
		Service: "api", Host: "web-1", Release: "0042-9f3ac1b", ConfigDrift: true,
	}})

	got := b.String()
	if !strings.Contains(got, "configuration differs") {
		t.Errorf("missing the finding:\n%s", got)
	}
	if !strings.Contains(got, "no longer matches what was deployed") {
		t.Errorf("should explain what was compared:\n%s", got)
	}
	if strings.Contains(got, "- ") || strings.Contains(got, "+ ") {
		t.Errorf("must not render a diff it does not have:\n%s", got)
	}
}

// "Could not determine" is not the same as "nothing wrong", and the output has
// to keep them apart.
func TestWriteDriftDistinguishesUnavailable(t *testing.T) {
	var b strings.Builder
	WriteDrift(&b, []DriftEntry{
		{Service: "api", Host: "box-1", Unavailable: "not deployed on this host"},
	})

	got := b.String()
	if !strings.Contains(got, "not deployed on this host") {
		t.Errorf("missing the reason:\n%s", got)
	}
	if strings.Contains(got, "matches release") {
		t.Errorf("an unknown service must not be reported as clean:\n%s", got)
	}
	if !strings.Contains(got, "nothing has drifted") {
		t.Errorf("an unavailable service is not drift:\n%s", got)
	}
}

func TestStatusRowsShowDrift(t *testing.T) {
	var b strings.Builder
	Status(&b, []StatusRow{
		{Service: "api", Host: "web-1", State: "running", Release: "0042-9f3ac1b"},
		{Service: "blog", Host: "web-1", State: "running", Release: "0001-aaaaaaa", Drift: "route"},
	})

	got := b.String()
	if !strings.Contains(got, "drift: route") {
		t.Errorf("drift should surface in the table:\n%s", got)
	}
}

// A host with no agent is not the same as a host with nothing wrong.
func TestStatusFooterStatesDegradation(t *testing.T) {
	var b strings.Builder
	StatusFooter(&b,
		map[string]string{"box-1": "no pilot agent on box-1"},
		map[string][]string{"web-1": {"api: service.down"}},
	)

	got := b.String()
	if !strings.Contains(got, "alert firing on web-1: api: service.down") {
		t.Errorf("firing alerts missing:\n%s", got)
	}
	if !strings.Contains(got, "drift and alerts are unavailable") {
		t.Errorf("degradation should be stated, not hidden:\n%s", got)
	}
	if !strings.Contains(got, "pilot bootstrap") {
		t.Errorf("should say how to fix it:\n%s", got)
	}
}

func TestStatusFooterSilentWhenAllIsWell(t *testing.T) {
	var b strings.Builder
	StatusFooter(&b, nil, nil)
	if b.String() != "" {
		t.Errorf("nothing to report should print nothing, got %q", b.String())
	}
}
