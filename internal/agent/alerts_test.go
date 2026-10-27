package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

func memRules(t *testing.T) []alert.Rule {
	t.Helper()
	cond, err := alert.Parse("host.mem.used_pct > 90")
	if err != nil {
		t.Fatal(err)
	}
	return []alert.Rule{{Cond: cond}}
}

func TestHostReadingCarriesMemory(t *testing.T) {
	a := newAgent(t)
	a.meminfo = func() ([]byte, error) {
		return []byte("MemTotal:        8000000 kB\nMemFree:          100000 kB\nMemAvailable:    2000000 kB\n"), nil
	}

	got := a.readings(t.Context(), memRules(t))[""].MemUsedPct
	if got == nil || *got != 75 {
		t.Errorf("MemUsedPct = %v, want 75 (MemAvailable counts as free, MemFree does not)", got)
	}
}

// A figure the agent could not read must stay unset rather than pass for 0% in
// use, which would resolve a memory alert that is still true.
func TestHostReadingLeavesUnreadMemoryUnset(t *testing.T) {
	tests := map[string]func() ([]byte, error){
		"no /proc":          func() ([]byte, error) { return nil, errors.New("no such file") },
		"zero total":        func() ([]byte, error) { return []byte("MemTotal: 0 kB\nMemAvailable: 0 kB\n"), nil },
		"no MemAvailable":   func() ([]byte, error) { return []byte("MemTotal: 8000000 kB\nMemFree: 100 kB\n"), nil },
		"garbage":           func() ([]byte, error) { return []byte("not meminfo"), nil },
		"available > total": func() ([]byte, error) { return []byte("MemTotal: 100 kB\nMemAvailable: 200 kB\n"), nil },
	}
	for name, read := range tests {
		t.Run(name, func(t *testing.T) {
			a := newAgent(t)
			a.meminfo = read
			if got := a.readings(t.Context(), memRules(t))[""].MemUsedPct; got != nil {
				t.Errorf("MemUsedPct = %d, want unset", *got)
			}
		})
	}
}

// MemAvailable can reach 0 on a host that has run out of memory, the case the
// rule exists for; that is 100% in use, not an unreadable figure.
func TestHostReadingExhaustedMemory(t *testing.T) {
	a := newAgent(t)
	a.meminfo = func() ([]byte, error) {
		return []byte("MemTotal: 8000000 kB\nMemAvailable: 0 kB\n"), nil
	}
	if got := a.readings(t.Context(), memRules(t))[""].MemUsedPct; got == nil || *got != 100 {
		t.Errorf("MemUsedPct = %v, want 100", got)
	}
}

// dfFake answers every Run with the same result; the embedded nil Executor
// makes any other call fail the test loudly.
type dfFake struct {
	transport.Executor
	res transport.Result
	err error
}

func (f dfFake) Run(context.Context, string) (transport.Result, error) { return f.res, f.err }

func TestHostReadingCarriesDisk(t *testing.T) {
	a := newAgent(t)
	a.Exec = dfFake{res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
		"/dev/sda1 100 86 14 86% /\n"}}

	r := a.readings(t.Context(), nil)[""]
	if r.DiskUsedPct == nil || *r.DiskUsedPct != 86 {
		t.Errorf("DiskUsedPct = %v, want 86", r.DiskUsedPct)
	}
}

// A disk figure df could not give must stay unset rather than pass for 0% in
// use, which would resolve a full-disk alert that is still true.
func TestHostReadingLeavesUnreadDiskUnset(t *testing.T) {
	tests := map[string]dfFake{
		"exec error":   {err: errors.New("fork failed")},
		"df failed":    {res: transport.Result{ExitCode: 1, Stderr: "df: /opt/pilot: No such file"}},
		"no output":    {},
		"garbage":      {res: transport.Result{Stdout: "not\ndf output"}},
		"header only":  {res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n"}},
		"dash percent": {res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\nnone 0 0 0 -% /\n"}},
		"negative":     {res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 86 14 -5% /\n"}},
		"over 100":     {res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 86 14 105% /\n"}},
	}
	for name, fake := range tests {
		t.Run(name, func(t *testing.T) {
			a := newAgent(t)
			a.Exec = fake
			if got := a.readings(t.Context(), nil)[""].DiskUsedPct; got != nil {
				t.Errorf("DiskUsedPct = %d, want unset", *got)
			}
		})
	}
}

// A memory rule that can never evaluate, e.g. in an LXC container reporting
// MemAvailable above MemTotal, should say so once rather than stay silent or
// repeat every sweep.
func TestUnreadMemoryIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	a := newAgent(t)
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	a.meminfo = func() ([]byte, error) { return []byte("MemTotal: 100 kB\nMemAvailable: 200 kB\n"), nil }
	a.readings(t.Context(), memRules(t))
	a.readings(t.Context(), memRules(t))

	out := buf.String()
	if n := strings.Count(out, "host memory unreadable"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "mem_total=102400") || !strings.Contains(out, "mem_available=204800") {
		t.Errorf("log should carry the figures it rejected:\n%s", out)
	}
}

// One bad expression is skipped with a warning on the agent's own logger, so
// a test can read it without touching the process-wide default.
func TestUnparseableRuleIsLogged(t *testing.T) {
	var buf bytes.Buffer
	a := newAgent(t)
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	a.fleet = &FleetConfig{Alerts: []config.Alert{{When: "no.such.metric > 1"}, {When: "host.disk.used_pct > 90"}}}

	if got := len(a.Rules()); got != 1 {
		t.Fatalf("got %d rules, want the parseable one", got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, `msg="skipping alert" when="no.such.metric > 1" err=`) {
		t.Errorf("want a warning naming the rule and its error, got:\n%s", out)
	}
}

// With no host.mem rule there is nothing for unreadable memory to break, so the
// agent should neither read it for alerting nor warn that such rules will not
// evaluate.
func TestUnreadMemoryIsQuietWithoutMemRule(t *testing.T) {
	var buf bytes.Buffer
	a := newAgent(t)
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	a.Exec = dfFake{err: errors.New("fork failed")}
	a.meminfo = func() ([]byte, error) {
		t.Error("memory read with no host.mem rule")
		return nil, errors.New("no such file")
	}
	a.fleet = &FleetConfig{Alerts: []config.Alert{{When: "host.disk.used_pct > 90"}}}

	if got := a.readings(t.Context(), a.Rules())[""].MemUsedPct; got != nil {
		t.Errorf("MemUsedPct = %d, want unset", *got)
	}
	if buf.Len() != 0 {
		t.Errorf("logged with no host.mem rule:\n%s", buf.String())
	}
}

func TestReadsMemory(t *testing.T) {
	tests := map[string]bool{
		"host.mem.used_pct > 90":  true,
		"host.mem.free_pct < 10":  true,
		"host.disk.used_pct > 90": false,
		"service.down":            false,
	}
	for when, want := range tests {
		cond, err := alert.Parse(when)
		if err != nil {
			t.Fatal(err)
		}
		if got := readsMemory([]alert.Rule{{Cond: cond}}); got != want {
			t.Errorf("readsMemory(%q) = %v, want %v", when, got, want)
		}
	}
	if readsMemory(nil) {
		t.Error("readsMemory(nil) = true, want false")
	}
}
