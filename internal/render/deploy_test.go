package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/deploy"
)

// The plan is what an operator, or an agent with the skill, reads before
// saying yes. A stopped service must not read as "unchanged", and a start with
// nothing to verify it must say so.
func TestPlanNamesAStart(t *testing.T) {
	plan := func(noGate bool) *deploy.Plan {
		return &deploy.Plan{
			Service:      &config.Service{Name: "kite"},
			NoHealthGate: noGate,
			Hosts: []deploy.HostPlan{{
				Host: "vps", From: "0042-9f3ac1b", To: "0042-9f3ac1b",
				Start: true, Observed: "stopped (all containers stopped)",
			}},
		}
	}

	tests := []struct {
		name   string
		noGate bool
		want   []string
	}{
		{"gated", false, []string{"start", "0042-9f3ac1b is stopped (all containers stopped)", "not rolled back"}},
		{"no health check", true, []string{"start", "no health check"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			Plan(&buf, plan(tc.noGate))
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("plan output lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "unchanged") {
				t.Errorf("a stopped service is not unchanged:\n%s", out)
			}
		})
	}
}

func TestOutcomesNameAStart(t *testing.T) {
	var buf bytes.Buffer
	Outcomes(&buf, []deploy.Outcome{{
		Service: "kite", Host: "vps", Release: "0042-9f3ac1b", From: "0042-9f3ac1b",
		Succeeded: true, Message: "started",
	}})
	if !strings.Contains(buf.String(), "started 0042-9f3ac1b again") {
		t.Errorf("outcome = %q", buf.String())
	}
}

// A stopped service the deploy will not start must still show as stopped, with
// the reason, rather than hide behind "unchanged".
func TestPlanNamesAStoppedServiceItWillNotStart(t *testing.T) {
	var buf bytes.Buffer
	Plan(&buf, &deploy.Plan{
		Service: &config.Service{Name: "kite"},
		Hosts: []deploy.HostPlan{{
			Host: "vps", From: "0042-9f3ac1b", To: "0042-9f3ac1b", NoOp: true,
			Observed:   "stopped (all containers stopped)",
			NotStarted: "blue-green is started by the agent, and vps has none",
		}},
	})
	for _, want := range []string{"unchanged", "stopped (all containers stopped)", "not started: blue-green"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("plan output lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestNothingToDoNamesAHostLeftStopped(t *testing.T) {
	plan := func(hosts ...deploy.HostPlan) *deploy.Plan {
		return &deploy.Plan{Service: &config.Service{Name: "kite"}, Hosts: hosts}
	}
	running := deploy.HostPlan{Host: "web-1", NoOp: true}
	stopped := deploy.HostPlan{
		Host: "vps", NoOp: true, Observed: "stopped (all containers stopped)",
		NotStarted: "blue-green is started by the agent, and vps has none",
	}

	tests := []struct {
		name    string
		plan    *deploy.Plan
		want    string
		wantNot string
	}{
		{"all running", plan(running), "nothing to do", "not started"},
		{"one left stopped", plan(running, stopped), "kite on vps is stopped (all containers stopped) and was not started", "nothing to do"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			NothingToDo(&buf, tc.plan)
			if !strings.Contains(buf.String(), tc.want) || strings.Contains(buf.String(), tc.wantNot) {
				t.Errorf("output = %q, want %q and not %q", buf.String(), tc.want, tc.wantNot)
			}
		})
	}
}

// What a deploy leaves for a human to clean up has to be on screen before they
// say yes, not discovered afterwards as a port clash.
func TestPlanShowsWarnings(t *testing.T) {
	var buf bytes.Buffer
	Plan(&buf, &deploy.Plan{
		Service: &config.Service{Name: "kite"},
		Hosts: []deploy.HostPlan{{
			Host: "vps", From: "0042-9f3ac1b", To: "0043-9f3ac1b",
			Changes:  []deploy.Change{{Field: "compose.project", From: "kite", To: "kite-2"}},
			Warnings: []string{"compose project kite → kite-2 leaves kite running"},
		}},
	})
	if !strings.Contains(buf.String(), "! warning  compose project kite → kite-2 leaves kite running") {
		t.Errorf("plan output lacks the warning:\n%s", buf.String())
	}
}
