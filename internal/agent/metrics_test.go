package agent

import (
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// Docker normalizes CPUPerc to one core; the sample must be a share of the
// whole host, or "20%" answers "20% of what?" with "bananas".
func TestComposeUsageNormalizesToHost(t *testing.T) {
	ndjson := `
{"Name":"wakapi-wakapi-1","CPUPerc":"120.00%","MemUsage":"256MiB / 8GiB"}
{"Name":"hopbox-docs-web-1","CPUPerc":"40.00%","MemUsage":"1.5GiB / 8GiB"}
{"Name":"hopbox-docs-worker-1","CPUPerc":"40.00%","MemUsage":"512MiB / 8GiB"}
{"Name":"unrelated-thing-1","CPUPerc":"99.00%","MemUsage":"1GiB / 8GiB"}
`
	projects := map[string]string{
		"wakapi": "wakapi",
		"docs":   "hopbox-docs",
	}
	out := composeUsage(ndjson, nil, projects, 4)

	w := out["wakapi"]
	if w.CPUPct != 30 { // 120% of one core / 4 cores
		t.Errorf("wakapi cpu = %v, want 30", w.CPUPct)
	}
	if w.MemBytes != 256<<20 {
		t.Errorf("wakapi mem = %d, want %d", w.MemBytes, 256<<20)
	}

	// Multi-container project sums; the hyphenated project name must claim
	// its own containers rather than losing them to a shorter prefix.
	d := out["docs"]
	if d.CPUPct != 20 {
		t.Errorf("docs cpu = %v, want 20", d.CPUPct)
	}
	gib := float64(1 << 30)
	if want := uint64(1.5*gib) + 512<<20; d.MemBytes != want {
		t.Errorf("docs mem = %d, want %d", d.MemBytes, want)
	}

	if _, ok := out["unrelated"]; ok {
		t.Error("containers outside known projects must not appear")
	}
}

func TestParseBytes(t *testing.T) {
	mib := float64(1 << 20)
	gib := float64(1 << 30)
	cases := map[string]uint64{
		"7.605MiB": uint64(7.605 * mib),
		"1.5GiB":   uint64(1.5 * gib),
		"512KiB":   512 << 10,
		"1.2GB":    12e8,
		"64B":      64,
		"garbage":  0,
	}
	for in, want := range cases {
		if got := parseBytes(in); got != want {
			t.Errorf("parseBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseUnitUsage(t *testing.T) {
	nsec, mem, ok := parseUnitUsage("CPUUsageNSec=1234567890\nMemoryCurrent=104857600\n")
	if !ok || nsec != 1234567890 || mem != 104857600 {
		t.Errorf("got nsec=%d mem=%d ok=%v", nsec, mem, ok)
	}
	// Accounting off: values are "[not set]".
	if _, _, ok := parseUnitUsage("CPUUsageNSec=[not set]\nMemoryCurrent=[not set]\n"); ok {
		t.Error("unparseable counters must not produce a sample")
	}
}

func TestParseProcStat(t *testing.T) {
	st, err := parseProcStat("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4\n")
	if err != nil {
		t.Fatal(err)
	}
	if st.total != 1000 || st.busy != 150 {
		t.Errorf("total=%d busy=%d, want 1000/150", st.total, st.busy)
	}

	next := procStat{busy: 250, total: 1400}
	pct, ok := next.busyPctSince(st)
	if !ok || pct != 25 {
		t.Errorf("busyPct = %v ok=%v, want 25", pct, ok)
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail := parseMeminfo("MemTotal:        8000000 kB\nMemFree:         100 kB\nMemAvailable:    6000000 kB\n")
	if total != 8000000*1024 || avail != 6000000*1024 {
		t.Errorf("total=%d avail=%d", total, avail)
	}
}

// The ring is bounded, and the since-filter returns only newer samples.
func TestMetricRingAndSince(t *testing.T) {
	a := &Agent{}
	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for i := range maxMetricSamples + 10 {
		a.recordServiceMetric("api", proto.MetricSample{At: base.Add(time.Duration(i) * time.Second)})
	}

	all, _, _ := a.MetricsSince(time.Time{})
	if len(all["api"]) != maxMetricSamples {
		t.Errorf("ring = %d, want %d", len(all["api"]), maxMetricSamples)
	}

	since, _, _ := a.MetricsSince(base.Add(time.Duration(maxMetricSamples+7) * time.Second))
	if len(since["api"]) != 2 {
		t.Errorf("since-filter returned %d samples, want 2", len(since["api"]))
	}
}

// The fleet that found this bug: `container_name:` pins strip the project
// prefix, and a stack's auxiliary container can be named anything at all.
// The compose label is ground truth; the prefix guess is only a fallback.
func TestComposeUsagePrefersLabels(t *testing.T) {
	ndjson := `
{"Name":"wakapi","CPUPerc":"40.00%","MemUsage":"64MiB / 8GiB"}
{"Name":"docmost_instance","CPUPerc":"40.00%","MemUsage":"256MiB / 8GiB"}
{"Name":"postgres_instance","CPUPerc":"40.00%","MemUsage":"128MiB / 8GiB"}
{"Name":"docmost-redis-1","CPUPerc":"40.00%","MemUsage":"32MiB / 8GiB"}
{"Name":"unlabeled-mystery","CPUPerc":"99.00%","MemUsage":"1GiB / 8GiB"}
`
	owners := containerProjects(`
{"Names":"wakapi","Labels":"com.docker.compose.project=wakapi,com.docker.compose.service=wakapi"}
{"Names":"docmost_instance","Labels":"com.docker.compose.project=docmost"}
{"Names":"postgres_instance","Labels":"com.docker.compose.project=docmost"}
{"Names":"docmost-redis-1","Labels":"com.docker.compose.project=docmost"}
`)
	projects := map[string]string{"wakapi": "wakapi", "docmost": "docmost"}
	out := composeUsage(ndjson, owners, projects, 4)

	if w := out["wakapi"]; w.CPUPct != 10 || w.MemBytes != 64<<20 {
		t.Errorf("pinned container missed via label: %+v", w)
	}
	// docmost = instance + its anonymous postgres + redis, all by label.
	d := out["docmost"]
	if d.CPUPct != 30 {
		t.Errorf("docmost cpu = %v, want 30 (three containers)", d.CPUPct)
	}
	if want := uint64(256+128+32) << 20; d.MemBytes != want {
		t.Errorf("docmost mem = %d, want %d", d.MemBytes, want)
	}
	if _, ok := out["unlabeled-mystery"]; ok {
		t.Error("a container with no label and no known prefix must not appear")
	}
}
