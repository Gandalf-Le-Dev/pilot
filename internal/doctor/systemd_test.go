package doctor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// fakeUnit answers `systemctl show` with canned properties.
type fakeUnit struct{ out string }

func (f fakeUnit) Run(context.Context, string) (transport.Result, error) {
	return transport.Result{Stdout: f.out}, nil
}

func unitSvc(timeout time.Duration) *config.Service {
	return &config.Service{
		Name:    "hopboxd",
		Runtime: config.RuntimeSystemd,
		Unit:    &config.Unit{Name: "hopboxd.service"},
		Health:  &config.Health{Systemd: true, Timeout: config.Duration(timeout)},
	}
}

func TestInspectUnitMissing(t *testing.T) {
	got := inspectUnit(context.Background(),
		fakeUnit{"LoadState=not-found\nTimeoutStopUSec=1min 30s\n"},
		unitSvc(2*time.Minute), "ks")

	if len(got) != 1 || got[0].Status != StatusFail {
		t.Fatalf("want one FAIL for a missing unit, got %+v", got)
	}
	if !strings.Contains(got[0].Title, "does not exist") {
		t.Errorf("title = %q", got[0].Title)
	}
}

// The hopbox drain footgun, generalised: a unit that is allowed 60s to shut
// down cleanly, behind a health gate that gives up in 30s. The gate expires
// while systemd is still legitimately stopping the old process, so a correct
// deploy is rolled back — and the rollback restarts the unit a second time
// while it was still draining.
func TestCheckStopBudget(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		stop    string
		want    bool
	}{
		{"gate shorter than the stop budget", 30 * time.Second, "1min 30s", true},
		{"gate exactly equal is still too short", 90 * time.Second, "1min 30s", true},
		{"gate comfortably longer", 5 * time.Minute, "1min 30s", false},
		{"no stop timeout reported", 30 * time.Second, "", false},
		// An unbounded stop budget cannot be exceeded, so there is nothing to
		// warn about — reporting it would be noise on every well-configured
		// unit that sets TimeoutStopSec=infinity deliberately.
		{"infinite stop budget", 30 * time.Second, "infinity", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkStopBudget(unitSvc(tc.timeout), "ks", tc.stop)
			if (got != nil) != tc.want {
				t.Fatalf("finding = %v, want finding: %v", got, tc.want)
			}
			if got != nil && got.Status != StatusFail {
				t.Errorf("status = %q, want FAIL", got.Status)
			}
		})
	}
}

// A service with no health check has no gate to expire, so the comparison is
// meaningless rather than merely passing.
func TestCheckStopBudgetIgnoresUngatedServices(t *testing.T) {
	s := unitSvc(0)
	s.Health = nil
	if f := checkStopBudget(s, "ks", "10min"); f != nil {
		t.Errorf("want no finding without a health check, got %+v", f)
	}
}

// deploy.Verify defaults to 60s when health.timeout is unset, so the check has
// to compare against that same default — otherwise a service inheriting the
// default would silently escape the check that exists for it.
func TestCheckStopBudgetUsesVerifyDefault(t *testing.T) {
	if f := checkStopBudget(unitSvc(0), "ks", "5min"); f == nil {
		t.Error("want a finding: the implied 60s gate is shorter than a 5min stop budget")
	}
	if f := checkStopBudget(unitSvc(0), "ks", "30s"); f != nil {
		t.Errorf("want no finding: the implied 60s gate exceeds a 30s stop budget, got %+v", f)
	}
}

func TestParseUSec(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"1min 30s", 90 * time.Second, true},
		{"2min", 2 * time.Minute, true},
		{"30s", 30 * time.Second, true},
		{"1h 5min", time.Hour + 5*time.Minute, true},
		{"90000000", 90 * time.Second, true}, // raw microseconds
		{"infinity", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseUSec(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("parseUSec(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestInspectUnitDisabled(t *testing.T) {
	got := inspectUnit(context.Background(),
		fakeUnit{"LoadState=loaded\nUnitFileState=disabled\nTimeoutStopUSec=10s\n"},
		unitSvc(5*time.Minute), "ks")

	if len(got) != 1 || got[0].Status != StatusWarn {
		t.Fatalf("want one WARN for a disabled unit, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, "reboot") {
		t.Errorf("detail should explain the consequence: %q", got[0].Detail)
	}
}

// A healthy unit produces nothing. Without this, a check that returned a
// finding unconditionally would pass every test above.
func TestInspectUnitClean(t *testing.T) {
	got := inspectUnit(context.Background(),
		fakeUnit{"LoadState=loaded\nUnitFileState=enabled\nTimeoutStopUSec=10s\n"},
		unitSvc(5*time.Minute), "ks")

	if len(got) != 0 {
		t.Errorf("want no findings for a healthy unit, got %+v", got)
	}
}
