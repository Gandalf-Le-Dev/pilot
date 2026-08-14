package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// Metrics sampling for the dashboard's resource panel.
//
// A separate, slower loop than state observation: reading cgroup numbers
// through `docker stats` costs a couple of seconds, which has no business on
// the 10-second state path. Samples live in a bounded in-memory ring and die
// with the agent — the same deliberate trade the state buffer makes, and the
// dashboard labels its charts accordingly. Anyone needing weeks of retention
// needs Prometheus, not a deploy tool.
const (
	MetricsInterval = 30 * time.Second

	// maxMetricSamples bounds each series: ~6 hours at 30s. Enough to answer
	// "what happened while I slept", small enough to never matter in memory.
	maxMetricSamples = 720
)

// metricsLoop samples per-service and host resource usage.
func (a *Agent) metricsLoop(ctx context.Context) {
	a.mu.Lock()
	a.capacity = proto.Capacity{Cores: runtime.NumCPU(), MemTotal: readMemTotal()}
	a.mu.Unlock()

	// CPU percentages are deltas of cumulative counters, so the first tick
	// only primes them and records nothing.
	var prevHost procStat
	prevUnits := map[string]uint64{}
	primed := false

	tick := time.NewTicker(MetricsInterval)
	defer tick.Stop()

	for {
		prevHost, primed = a.collectMetrics(ctx, prevHost, prevUnits, primed)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *Agent) collectMetrics(ctx context.Context, prevHost procStat, prevUnits map[string]uint64, primed bool) (procStat, bool) {
	now := time.Now().UTC()

	// Host usage from /proc — the agent runs on the machine it measures.
	host, herr := readProcStat()
	if herr == nil && primed {
		used := readMemUsed()
		if pct, ok := host.busyPctSince(prevHost); ok {
			a.recordHostMetric(proto.MetricSample{At: now, CPUPct: pct, MemBytes: used})
		}
	}

	// One `docker stats` for the whole host, bucketed by compose project,
	// rather than one per service: the call costs seconds, the bucketing is
	// free.
	projects := map[string]string{} // service -> compose project
	units := map[string]string{}    // service -> systemd unit
	for _, name := range a.ServiceNames() {
		s, ok := a.Service(name)
		if !ok {
			continue
		}
		switch {
		case s.Runtime == config.RuntimeCompose && s.Compose != nil:
			projects[name] = s.Compose.Project
		case s.Runtime == config.RuntimeSystemd && s.Unit != nil:
			units[name] = s.Unit.Name
		}
		// Static services have no process; the panel says so instead of
		// charting zeros.
	}

	cores := runtime.NumCPU()

	if len(projects) > 0 {
		res, err := a.Exec.Run(ctx, "docker stats --no-stream --format json 2>/dev/null")
		if err == nil && res.OK() {
			for svc, sample := range composeUsage(res.Stdout, projects, cores) {
				sample.At = now
				a.recordServiceMetric(svc, sample)
			}
		}
	}

	for svc, unit := range units {
		res, err := a.Exec.Run(ctx, "systemctl show "+unit+" -p CPUUsageNSec,MemoryCurrent")
		if err != nil || !res.OK() {
			continue
		}
		nsec, mem, ok := parseUnitUsage(res.Stdout)
		if !ok {
			continue
		}
		prev, seen := prevUnits[unit]
		prevUnits[unit] = nsec
		if !seen {
			continue // first sight of this unit: prime the counter
		}
		elapsed := float64(MetricsInterval.Nanoseconds())
		pct := float64(nsec-prev) / elapsed / float64(cores) * 100
		if nsec < prev { // unit restarted; counter reset
			pct = 0
		}
		a.recordServiceMetric(svc, proto.MetricSample{At: now, CPUPct: pct, MemBytes: mem})
	}

	if herr == nil {
		return host, true
	}
	return prevHost, primed
}

func (a *Agent) recordServiceMetric(name string, s proto.MetricSample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.serviceMetrics == nil {
		a.serviceMetrics = map[string][]proto.MetricSample{}
	}
	ring := append(a.serviceMetrics[name], s)
	if len(ring) > maxMetricSamples {
		ring = ring[len(ring)-maxMetricSamples:]
	}
	a.serviceMetrics[name] = ring
}

func (a *Agent) recordHostMetric(s proto.MetricSample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hostMetrics = append(a.hostMetrics, s)
	if len(a.hostMetrics) > maxMetricSamples {
		a.hostMetrics = a.hostMetrics[len(a.hostMetrics)-maxMetricSamples:]
	}
}

// MetricsSince returns each series' samples after the cutoff, plus the host
// series and the capacity. A zero cutoff returns everything held.
func (a *Agent) MetricsSince(since time.Time) (map[string][]proto.MetricSample, []proto.MetricSample, proto.Capacity) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	services := map[string][]proto.MetricSample{}
	for name, ring := range a.serviceMetrics {
		services[name] = samplesAfter(ring, since)
	}
	return services, samplesAfter(a.hostMetrics, since), a.capacity
}

func samplesAfter(ring []proto.MetricSample, since time.Time) []proto.MetricSample {
	i := 0
	for i < len(ring) && !ring[i].At.After(since) {
		i++
	}
	return append([]proto.MetricSample(nil), ring[i:]...)
}

// --- docker stats ---

// dockerStat is one line of `docker stats --no-stream --format json`.
type dockerStat struct {
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
}

// composeUsage buckets a whole host's `docker stats` output by compose
// project and sums each service's containers.
//
// Containers are matched by project-name prefix (`<project>-<service>-<n>`,
// or `_` on legacy compose), longest project first so "hopbox-docs" claims
// its containers before a project named "hopbox" could. Docker's CPUPerc is
// normalized to one core — a busy 4-core host reads 400% — so the sum is
// divided by the core count to make it a share of the host.
func composeUsage(ndjson string, projects map[string]string, cores int) map[string]proto.MetricSample {
	type acc struct {
		cpu float64
		mem uint64
	}

	byLen := make([]string, 0, len(projects)) // distinct project names
	seen := map[string]bool{}
	for _, p := range projects {
		if !seen[p] {
			seen[p] = true
			byLen = append(byLen, p)
		}
	}
	sort.Slice(byLen, func(i, j int) bool { return len(byLen[i]) > len(byLen[j]) })

	byProject := map[string]*acc{}
	for line := range strings.SplitSeq(ndjson, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var st dockerStat
		if err := json.Unmarshal([]byte(line), &st); err != nil {
			continue
		}
		for _, project := range byLen {
			if !strings.HasPrefix(st.Name, project+"-") && !strings.HasPrefix(st.Name, project+"_") {
				continue
			}
			if byProject[project] == nil {
				byProject[project] = &acc{}
			}
			byProject[project].cpu += parsePercent(st.CPUPerc)
			byProject[project].mem += parseMemUsage(st.MemUsage)
			break
		}
	}

	if cores < 1 {
		cores = 1
	}
	out := map[string]proto.MetricSample{}
	for svc, project := range projects {
		if u := byProject[project]; u != nil {
			out[svc] = proto.MetricSample{CPUPct: u.cpu / float64(cores), MemBytes: u.mem}
		}
	}
	return out
}

// parsePercent reads docker's "1.23%" form.
func parsePercent(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	if err != nil {
		return 0
	}
	return v
}

// parseMemUsage reads the used side of docker's "7.6MiB / 7.8GiB".
func parseMemUsage(s string) uint64 {
	used, _, _ := strings.Cut(s, "/")
	return parseBytes(strings.TrimSpace(used))
}

// parseBytes reads docker's human sizes, both binary (MiB) and decimal (MB).
func parseBytes(s string) uint64 {
	units := []struct {
		suffix string
		factor float64
	}{
		// Longest suffixes first, so "MiB" is not read as "B".
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"KB", 1e3},
		{"B", 1},
	}
	s = strings.TrimSpace(s)
	for _, u := range units {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil || v < 0 {
				return 0
			}
			return uint64(v * u.factor)
		}
	}
	return 0
}

// --- systemd ---

// parseUnitUsage reads `systemctl show -p CPUUsageNSec,MemoryCurrent` output.
// Either property may be "[not set]" on units without accounting.
func parseUnitUsage(out string) (nsec, mem uint64, ok bool) {
	for line := range strings.SplitSeq(out, "\n") {
		key, val, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		n, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			continue // "[not set]"
		}
		switch key {
		case "CPUUsageNSec":
			nsec, ok = n, true
		case "MemoryCurrent":
			mem = n
		}
	}
	return nsec, mem, ok
}

// --- /proc ---

// procStat is the host's cumulative CPU counters.
type procStat struct {
	busy, total uint64
}

// busyPctSince turns two cumulative readings into a percentage.
func (p procStat) busyPctSince(prev procStat) (float64, bool) {
	dTotal := p.total - prev.total
	if prev.total == 0 || p.total < prev.total || dTotal == 0 {
		return 0, false
	}
	return float64(p.busy-prev.busy) / float64(dTotal) * 100, true
}

func readProcStat() (procStat, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return procStat{}, err
	}
	return parseProcStat(string(b))
}

// parseProcStat reads the aggregate "cpu" line. Busy is everything except
// idle and iowait.
func parseProcStat(s string) (procStat, error) {
	for line := range strings.SplitSeq(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var vals []uint64
		for _, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				break
			}
			vals = append(vals, v)
		}
		if len(vals) < 4 {
			return procStat{}, fmt.Errorf("short cpu line: %q", line)
		}
		var st procStat
		for i, v := range vals {
			st.total += v
			if i != 3 && i != 4 { // idle, iowait
				st.busy += v
			}
		}
		return st, nil
	}
	return procStat{}, fmt.Errorf("no cpu line in /proc/stat")
}

func readMemTotal() uint64 {
	total, _ := readMeminfo()
	return total
}

func readMemUsed() uint64 {
	total, avail := readMeminfo()
	if avail > total {
		return 0
	}
	return total - avail
}

func readMeminfo() (total, available uint64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	return parseMeminfo(string(b))
}

// parseMeminfo reads MemTotal and MemAvailable, which /proc reports in kB.
func parseMeminfo(s string) (total, available uint64) {
	for line := range strings.SplitSeq(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			available = v * 1024
		}
	}
	return total, available
}
