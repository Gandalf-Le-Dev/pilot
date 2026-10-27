package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

func TestHostReadingCarriesMemory(t *testing.T) {
	a := newAgent(t)
	a.meminfo = func() ([]byte, error) {
		return []byte("MemTotal:        8000000 kB\nMemFree:          100000 kB\nMemAvailable:    2000000 kB\n"), nil
	}

	got := a.readings(t.Context())[""].MemUsedPct
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
			if got := a.readings(t.Context())[""].MemUsedPct; got != nil {
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
	if got := a.readings(t.Context())[""].MemUsedPct; got == nil || *got != 100 {
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

	r := a.readings(t.Context())[""]
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
			if got := a.readings(t.Context())[""].DiskUsedPct; got != nil {
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
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := newAgent(t)
	a.meminfo = func() ([]byte, error) { return []byte("MemTotal: 100 kB\nMemAvailable: 200 kB\n"), nil }
	a.readings(t.Context())
	a.readings(t.Context())

	out := buf.String()
	if n := strings.Count(out, "host memory unreadable"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "mem_total=102400") || !strings.Contains(out, "mem_available=204800") {
		t.Errorf("log should carry the figures it rejected:\n%s", out)
	}
}
