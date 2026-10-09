package statuspage

import (
	"cmp"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"slices"
	"time"
)

// Page is the public status: the body of /status.json, and what / renders.
//
// Built only from the server's configuration (title, labels) and from enum
// states and times. Host names, service names behind a label, and anything an
// agent could phrase have no field here to land in.
type Page struct {
	Title    string    `json:"title"`
	State    State     `json:"state"`
	Updated  time.Time `json:"updated"`
	Services []Service `json:"services"`
}

// Service is one row on the page.
type Service struct {
	Label string `json:"label"`
	State State  `json:"state"`

	// NotReportingSince is the last time the service's host was heard from,
	// set only when State is NotReporting.
	NotReportingSince time.Time `json:"not_reporting_since,omitzero"`

	// UptimePct is the share of the day's observed buckets that were up or
	// degraded. Nil until a bucket has been observed.
	UptimePct *float64 `json:"uptime_pct_24h,omitempty"`

	History History `json:"history"`
}

// Assemble builds the page from one row per (host, service).
//
// Rows sharing a label are one service to the public — the same site served
// from two hosts — so they merge: the worse state wins, and so does the worse
// bucket. Showing two "Website" rows would publish the fleet's topology,
// which is exactly what hiding host names is for.
func Assemble(title string, now time.Time, rows []Service) Page {
	byLabel := map[string]*Service{}
	histories := map[string][]History{}
	for _, r := range rows {
		histories[r.Label] = append(histories[r.Label], r.History)
		s, ok := byLabel[r.Label]
		if !ok {
			row := r
			byLabel[r.Label] = &row
			continue
		}
		if severity(r.State) > severity(s.State) {
			s.State = r.State
			s.NotReportingSince = r.NotReportingSince
		}
		// Two silent hosts behind one label: the service has been dark
		// since the first of them went quiet.
		if r.State == NotReporting && s.State == NotReporting && !r.NotReportingSince.IsZero() &&
			(s.NotReportingSince.IsZero() || r.NotReportingSince.Before(s.NotReportingSince)) {
			s.NotReportingSince = r.NotReportingSince
		}
	}

	p := Page{Title: title, State: Up, Updated: now.UTC()}
	for label, s := range byLabel {
		s.History = mergeHistory(histories[label])
		s.UptimePct = uptime(s.History)
		if s.State != NotReporting {
			s.NotReportingSince = time.Time{}
		}
		p.State = Worse(p.State, s.State)
		p.Services = append(p.Services, *s)
	}
	slices.SortFunc(p.Services, func(a, b Service) int { return cmp.Compare(a.Label, b.Label) })
	return p
}

// mergeHistory lays several histories over one another, bucket by bucket.
// Buckets align to the epoch on every agent, so the same start means the
// same five minutes everywhere.
func mergeHistory(hs []History) History {
	width := int64(BucketWidth / time.Second)
	merged := map[int64]State{}
	var newest int64
	for _, h := range hs {
		for i, st := range h.States {
			start := h.End - int64(len(h.States)-1-i)*width
			if prev, ok := merged[start]; ok {
				st = Worse(prev, st)
			}
			merged[start] = st
			newest = max(newest, start)
		}
	}
	if len(merged) == 0 {
		return History{}
	}

	oldest := newest
	for start := range merged {
		oldest = min(oldest, start)
	}
	oldest = max(oldest, newest-width*(HistoryBuckets-1))

	out := History{End: newest}
	for start := oldest; start <= newest; start += width {
		st, ok := merged[start]
		if !ok {
			st = Unknown
		}
		out.States = append(out.States, st)
	}
	return out
}

func uptime(h History) *float64 {
	var available, observed int
	for _, st := range h.States {
		switch st {
		case Up, Degraded:
			available++
			observed++
		case Down:
			observed++
		}
	}
	if observed == 0 {
		return nil
	}
	pct := float64(available) / float64(observed) * 100
	return &pct
}

// barCount is how many bars the page draws for a day: half an hour each.
// The JSON keeps every five-minute bucket; the page only needs a shape.
const barCount = 48

// bars folds the last 24 hours into barCount bars, worst state first. A bar
// with no bucket at all is empty rather than unknown, so a service that
// joined the page an hour ago does not show a day of grey.
func bars(h History, now time.Time) []State {
	width := int64(BucketWidth / time.Second)
	per := int64(HistoryBuckets / barCount)
	newest := BucketStart(now)
	oldest := newest - width*(HistoryBuckets-1)

	have := map[int64]State{}
	for i, st := range h.States {
		have[h.End-int64(len(h.States)-1-i)*width] = st
	}

	out := make([]State, barCount)
	for b := range int64(barCount) {
		for k := range per {
			if st, ok := have[oldest+(b*per+k)*width]; ok {
				if out[b] == "" {
					out[b] = st
				} else {
					out[b] = Worse(out[b], st)
				}
			}
		}
	}
	return out
}

//go:embed page.css
var css string

// ContentSecurityPolicy is the header the page must be served with. The page
// has no script at all, and its one stylesheet is admitted by hash rather
// than by 'unsafe-inline', so an injected <style> or <script> is inert even
// if escaping were ever bypassed.
var ContentSecurityPolicy = func() string {
	sum := sha256.Sum256([]byte(css))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

//go:embed page.html
var pageHTML string

var pageTemplate = template.Must(template.New("page").Funcs(template.FuncMap{
	"stateText": stateText,
	"utc":       func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	"pct":       func(p *float64) string { return fmt.Sprintf("%.2f%%", *p) },
}).Parse(pageHTML))

type pageView struct {
	Page
	CSS     template.CSS
	Summary string
	Rows    []rowView
}

type rowView struct {
	Service
	Bars []State
}

// Render writes the page as HTML.
func Render(w io.Writer, p Page) error {
	v := pageView{Page: p, CSS: template.CSS(css), Summary: summary(p)}
	for _, s := range p.Services {
		v.Rows = append(v.Rows, rowView{Service: s, Bars: bars(s.History, p.Updated)})
	}
	return pageTemplate.Execute(w, v)
}

func stateText(s State) string {
	switch s {
	case Up:
		return "Up"
	case Degraded:
		return "Degraded"
	case Down:
		return "Down"
	case AwaitingReport:
		return "Awaiting report"
	case NotReporting:
		return "Not reporting"
	}
	return "Unknown"
}

func summary(p Page) string {
	if len(p.Services) == 0 {
		return "No services are listed"
	}
	switch p.State {
	case Up:
		return "All services are up"
	case AwaitingReport:
		return "Waiting for the first reports"
	case Degraded:
		return "Some services are degraded"
	case NotReporting:
		return "Some services are not reporting"
	case Down:
		return "Some services are down"
	}
	return "Some services cannot be checked"
}
