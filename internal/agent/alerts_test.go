package agent

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
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
