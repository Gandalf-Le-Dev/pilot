// Package views renders the dashboard's HTML. It knows nothing about SSH or
// polling: the server hands it these view models and it draws them.
package views

import (
	"fmt"
	"sort"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/dashboard/components/chart"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// Fleet is everything one refresh shows.
type Fleet struct {
	Hosts       []Host
	GeneratedAt time.Time
}

// Host is one machine's slice of every panel.
type Host struct {
	Name string

	// Err is set when the host has no usable agent; the panels say so
	// instead of pretending an empty host is a healthy one.
	Err string

	DiskUsed   int
	Capacity   proto.Capacity
	Services   []Service
	HostSeries []proto.MetricSample
	Alerts     []proto.AlertEvent
	Deploys    []Deploy
}

// Service is one row of the status panel plus its resource series.
type Service struct {
	Name    string
	Runtime string
	Manage  string
	State   string
	Release string
	Detail  string
	Drift   bool
	Series  []proto.MetricSample
}

// Deploy is one line of history, already flattened per host.
type Deploy struct {
	Host       string
	Service    string
	Release    string
	By         string
	Outcome    string
	FinishedAt time.Time
}

// SeriesData turns samples into chart rows. CPU is already a share of the
// host; memory becomes one so both series share the 0–100 axis — the
// absolute values live in the labels next to the chart.
func SeriesData(samples []proto.MetricSample, cap proto.Capacity) []chart.Datum {
	out := make([]chart.Datum, 0, len(samples))
	for _, s := range samples {
		mem := 0.0
		if cap.MemTotal > 0 {
			mem = float64(s.MemBytes) / float64(cap.MemTotal) * 100
		}
		out = append(out, chart.Datum{
			"t":   s.At.Local().Format("15:04"),
			"cpu": round1(s.CPUPct),
			"mem": round1(mem),
		})
	}
	return out
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// Latest returns the newest sample, for the absolute figures beside a chart.
func Latest(samples []proto.MetricSample) (proto.MetricSample, bool) {
	if len(samples) == 0 {
		return proto.MetricSample{}, false
	}
	return samples[len(samples)-1], true
}

// FmtBytes renders a byte count the way an operator reads one.
func FmtBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(b)/float64(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

// FmtPct renders a share with one decimal.
func FmtPct(v float64) string { return fmt.Sprintf("%.1f%%", v) }

// FmtAgo renders how long ago something happened, coarsely: the panel answers
// "last night or last month", not "how many seconds".
func FmtAgo(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// RecentDeploys flattens every host's history into one newest-first list.
func RecentDeploys(f Fleet, limit int) []Deploy {
	var all []Deploy
	for _, h := range f.Hosts {
		all = append(all, h.Deploys...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].FinishedAt.After(all[j].FinishedAt) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

func anyAlerts(f Fleet) bool {
	for _, h := range f.Hosts {
		if len(h.Alerts) > 0 {
			return true
		}
	}
	return false
}

func anyDeploys(f Fleet) bool {
	for _, h := range f.Hosts {
		if len(h.Deploys) > 0 {
			return true
		}
	}
	return false
}
