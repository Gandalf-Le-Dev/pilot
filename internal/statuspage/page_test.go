package statuspage

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var noon = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func hist(end time.Time, states ...State) History {
	return History{End: BucketStart(end), States: states}
}

// Two hosts serving one label are one service to the public: the worse
// state and the worse bucket win, and no second row gives the topology away.
func TestAssembleMergesRowsByLabel(t *testing.T) {
	p := Assemble("mroc.me", noon, []Service{
		{Label: "Website", State: Up, History: hist(noon, Up, Up, Up)},
		{Label: "Website", State: Degraded, History: hist(noon.Add(-BucketWidth), Down, Up)},
		{Label: "Notes", State: Up, History: hist(noon, Up)},
	})

	if len(p.Services) != 2 || p.Services[0].Label != "Notes" || p.Services[1].Label != "Website" {
		t.Fatalf("services = %+v, want Notes and Website once each, sorted", p.Services)
	}
	site := p.Services[1]
	if site.State != Degraded {
		t.Errorf("merged state = %s, want the worse of up and degraded", site.State)
	}
	if want := []State{Down, Up, Up}; !slices.Equal(site.History.States, want) {
		t.Errorf("merged history = %v, want %v aligned on bucket times", site.History.States, want)
	}
	if site.UptimePct == nil || *site.UptimePct < 66 || *site.UptimePct > 67 {
		t.Errorf("uptime = %v, want two of three buckets", site.UptimePct)
	}
	if p.State != Degraded {
		t.Errorf("page state = %s, want the worst service", p.State)
	}
}

func TestAssembleKeepsSinceOnlyWhenNotReporting(t *testing.T) {
	since := noon.Add(-10 * time.Minute)
	p := Assemble("t", noon, []Service{
		{Label: "A", State: NotReporting, NotReportingSince: since},
		{Label: "B", State: Up, NotReportingSince: since},
	})
	if !p.Services[0].NotReportingSince.Equal(since) {
		t.Error("a silent service lost its since")
	}
	if !p.Services[1].NotReportingSince.IsZero() {
		t.Error("a reporting service carries a since")
	}
	p = Assemble("t", noon, []Service{
		{Label: "A", State: NotReporting, NotReportingSince: since.Add(5 * time.Minute)},
		{Label: "A", State: NotReporting, NotReportingSince: since},
		{Label: "A", State: NotReporting, NotReportingSince: since.Add(time.Minute)},
	})
	if got := p.Services[0].NotReportingSince; !got.Equal(since) {
		t.Errorf("merged since = %s, want the earliest %s", got, since)
	}
	if p.Services[0].UptimePct != nil {
		t.Error("uptime with no observed bucket should be absent, not zero")
	}
}

func TestBarsFoldTheDay(t *testing.T) {
	h := hist(noon, Up, Up, Down, Up, Up, Up, Up)
	b := bars(h, noon)
	if len(b) != barCount {
		t.Fatalf("got %d bars, want %d", len(b), barCount)
	}
	if b[0] != "" {
		t.Errorf("a bar with no data reads %q, want empty", b[0])
	}
	// The last bar holds the six buckets ending at noon (11:35 to 12:00), so
	// the down bucket at 11:40 darkens it and 11:30 falls in the bar before.
	if b[barCount-2] != Up || b[barCount-1] != Down {
		t.Errorf("last bars = %v, want [up down]", b[barCount-2:])
	}
}

// The CSP admits the stylesheet by hash, so the bytes in <style> must be
// exactly the bytes hashed. A template change that adds whitespace there
// would leave the page unstyled in every browser.
func TestRenderedStyleMatchesThePolicyHash(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, Assemble("mroc.me", noon, []Service{{Label: "Notes", State: Up, History: hist(noon, Up)}})); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(buf.String())
	if m == nil {
		t.Fatal("no <style> in the page")
	}
	sum := sha256.Sum256([]byte(m[1]))
	if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(ContentSecurityPolicy, want) {
		t.Errorf("CSP %q does not admit the rendered style (%s)", ContentSecurityPolicy, want)
	}
	if strings.Contains(buf.String(), "<script") {
		t.Error("the page carries a script")
	}
}

func TestRenderEscapesLabels(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, Assemble(`<b>x</b>`, noon, []Service{{Label: `<img src=x onerror=alert(1)>`, State: Up}})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "<img") || strings.Contains(buf.String(), "<b>x") {
		t.Errorf("a label reached the page unescaped:\n%s", buf.String())
	}
}

// Every state the page can be in has a summary border and a state colour, so
// a silent fleet does not render with the neutral border of a healthy one.
func TestEveryStateIsStyled(t *testing.T) {
	for _, st := range []State{Up, Degraded, Down, Unknown, AwaitingReport, NotReporting} {
		for _, prefix := range []string{".p-", ".t-"} {
			if !strings.Contains(css, prefix+string(st)) {
				t.Errorf("page.css has no %s%s", prefix, st)
			}
		}
	}
}
