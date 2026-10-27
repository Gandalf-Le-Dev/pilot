package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func diskRules(t *testing.T) []alert.Rule {
	t.Helper()
	cond, err := alert.Parse("host.disk.used_pct > 90")
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

// 0% and 100% are the bounds of a real reading, and a full disk is the case
// the rule exists for.
func TestHostReadingCarriesDisk(t *testing.T) {
	for _, want := range []int{86, 100, 0} {
		t.Run(fmt.Sprintf("%d%%", want), func(t *testing.T) {
			a := newAgent(t)
			a.Exec = dfFake{res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
				fmt.Sprintf("/dev/sda1 100 %d %d %d%% /\n", want, 100-want, want)}}

			r := a.readings(t.Context(), nil)[""]
			if r.DiskUsedPct == nil || *r.DiskUsedPct != want {
				t.Errorf("DiskUsedPct = %v, want %d", r.DiskUsedPct, want)
			}
		})
	}
}

// A disk figure df could not give must stay unset rather than pass for 0% in
// use, which would resolve a full-disk alert that is still true.
func TestHostReadingLeavesUnreadDiskUnset(t *testing.T) {
	tests := map[string]dfFake{
		"exec error":             {err: errors.New("fork failed")},
		"df failed":              {res: transport.Result{ExitCode: 1, Stderr: "df: /opt/pilot: No such file"}},
		"df failed after output": {res: transport.Result{ExitCode: 1, Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 34 66 34% /\n"}},
		"no output":              {},
		"garbage":                {res: transport.Result{Stdout: "not\ndf output"}},
		"header only":            {res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n"}},
		"dash percent":           {res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\nnone 0 0 0 -% /\n"}},
		"negative":               {res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 86 14 -5% /\n"}},
		"over 100":               {res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 86 14 105% /\n"}},
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

// With the Pilot root gone, the volume that held it is unknown; reading the
// parent instead would report another disk's headroom as Pilot's.
func TestDiskUnreadWhenRootMissing(t *testing.T) {
	a := newAgent(t)
	d := a.diskUsage(t.Context())
	if d == nil || d.Path != a.Layout.Root {
		t.Fatalf("diskUsage = %+v, want a reading for %s", d, a.Layout.Root)
	}

	if err := os.RemoveAll(a.Layout.Root); err != nil {
		t.Fatal(err)
	}
	if d := a.diskUsage(t.Context()); d != nil {
		t.Errorf("diskUsage = %+v with the root missing, want nil", d)
	}
}

// hungDF stands in for df on a mount that never answers.
type hungDF struct{ transport.Executor }

func (hungDF) Run(ctx context.Context, _ string) (transport.Result, error) {
	<-ctx.Done()
	return transport.Result{}, ctx.Err()
}

func TestHungDiskReadIsBounded(t *testing.T) {
	saved := diskTimeout
	diskTimeout = 50 * time.Millisecond
	t.Cleanup(func() { diskTimeout = saved })

	a := newAgent(t)
	a.Exec = hungDF{}
	done := make(chan *int, 1)
	go func() { done <- a.readings(t.Context(), nil)[""].DiskUsedPct }()

	select {
	case got := <-done:
		if got != nil {
			t.Errorf("DiskUsedPct = %d, want unset", *got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readings blocked on a hung df")
	}
}

// A disk rule that can never evaluate should say so once, naming the path
// and what df said, rather than stay silent or repeat every sweep.
func TestUnreadDiskIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	a := newAgent(t)
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	a.Exec = dfFake{res: transport.Result{ExitCode: 1, Stderr: "df: /opt/pilot: No such file or directory\n"}}
	a.readings(t.Context(), diskRules(t))
	a.readings(t.Context(), diskRules(t))

	out := buf.String()
	if n := strings.Count(out, "host disk unreadable"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, out)
	}
	for _, want := range []string{"path=" + a.Layout.Root, "exit_code=1", `stderr="df: /opt/pilot: No such file or directory"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log should carry %s:\n%s", want, out)
		}
	}
}

// The warning is about alert rules, so only a sweep that has a disk rule to
// lose gives it: not one without, and not a status or dashboard read.
func TestUnreadDiskIsQuietWithoutDiskRule(t *testing.T) {
	var buf bytes.Buffer
	a := newAgent(t)
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	a.Exec = dfFake{err: errors.New("fork failed")}
	a.fleet = &FleetConfig{Alerts: []config.Alert{{When: "host.disk.used_pct > 90"}}}

	a.readings(t.Context(), nil)
	if d := a.diskUsage(t.Context()); d != nil {
		t.Errorf("diskUsage = %+v, want nil", d)
	}
	if strings.Contains(buf.String(), "host disk unreadable") {
		t.Errorf("warned with no disk rule in the sweep:\n%s", buf.String())
	}
}

// A sweep canceled mid-read, e.g. at shutdown, is not a disk fault, and must
// not spend the one warning a real fault gets.
func TestCanceledDiskReadIsNotLogged(t *testing.T) {
	var buf bytes.Buffer
	a := newAgent(t)
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	a.Exec = hungDF{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if got := a.readings(ctx, diskRules(t))[""].DiskUsedPct; got != nil {
		t.Errorf("DiskUsedPct = %d, want unset", *got)
	}
	if buf.Len() != 0 {
		t.Errorf("logged a canceled read:\n%s", buf.String())
	}
}

func TestReadsDisk(t *testing.T) {
	tests := map[string]bool{
		"host.disk.used_pct > 90": true,
		"host.disk.free_pct < 10": true,
		"host.mem.used_pct > 90":  false,
		"service.down":            false,
	}
	for when, want := range tests {
		cond, err := alert.Parse(when)
		if err != nil {
			t.Fatal(err)
		}
		if got := readsDisk([]alert.Rule{{Cond: cond}}); got != want {
			t.Errorf("readsDisk(%q) = %v, want %v", when, got, want)
		}
	}
	if readsDisk(nil) {
		t.Error("readsDisk(nil) = true, want false")
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

// Rules runs every evaluation, so a bad rule left in the config must not warn
// every fifteen seconds. Once it leaves the config it is forgotten, and warns
// again if it comes back.
func TestUnparseableRuleIsLoggedOncePerAppearance(t *testing.T) {
	var buf bytes.Buffer
	a := newAgent(t)
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	bad := &FleetConfig{Alerts: []config.Alert{{When: "no.such.metric > 1"}}}
	good := &FleetConfig{Alerts: []config.Alert{{When: "host.disk.used_pct > 90"}}}

	for _, fc := range []*FleetConfig{bad, bad, bad, good, bad, bad} {
		a.fleet = fc
		a.Rules()
	}
	if n := strings.Count(buf.String(), "skipping alert"); n != 2 {
		t.Errorf("logged %d times, want once per appearance:\n%s", n, buf.String())
	}
}

// With no host.mem rule there is nothing for unreadable memory to break, so the
// agent should neither read it for alerting nor warn that such rules will not
// evaluate.
func TestUnreadMemoryIsQuietWithoutMemRule(t *testing.T) {
	var buf bytes.Buffer
	a := newAgent(t)
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	a.Exec = dfFake{res: transport.Result{Stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 34 66 34% /\n"}}
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

// The warning is the only trace of why memory rules are stuck, so it must
// name the cause rather than report figures that were never read.
func TestUnreadMemoryWarningCarriesTheCause(t *testing.T) {
	tests := map[string]struct {
		read func() ([]byte, error)
		want string
	}{
		"read error": {
			func() ([]byte, error) { return nil, errors.New("permission denied") },
			`err="permission denied"`,
		},
		"no MemAvailable": {
			func() ([]byte, error) { return []byte("MemTotal: 8000000 kB\n"), nil },
			`err="MemTotal or MemAvailable missing from /proc/meminfo"`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			a := newAgent(t)
			a.logger = slog.New(slog.NewTextHandler(&buf, nil))
			a.meminfo = tt.read
			a.readings(t.Context(), memRules(t))

			out := buf.String()
			if !strings.Contains(out, tt.want) {
				t.Errorf("want %s in:\n%s", tt.want, out)
			}
			if strings.Contains(out, "mem_total=") {
				t.Errorf("figures that were never read should not be logged:\n%s", out)
			}
		})
	}
}

// An unreadable fleet config leaves the host with no host rules and no
// notifiers, so it is an error, not a warning.
func TestUnreadableFleetConfigIsAnError(t *testing.T) {
	var buf bytes.Buffer
	a := newAgent(t)
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	if err := os.MkdirAll(a.cacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.cacheDir(), FleetConfigFile), []byte("no_such_field: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.loadFleetConfig(); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, `level=ERROR msg="ignoring unreadable fleet config" file=_fleet.yaml err=`) {
		t.Errorf("got %q, want an error naming the file", out)
	}
}
