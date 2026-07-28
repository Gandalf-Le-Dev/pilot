package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gandalfledev/pilot/internal/config"
)

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		findings []Finding
		want     int
	}{
		{"clean", []Finding{{Status: StatusOK}}, 0},
		{"warnings only", []Finding{{Status: StatusOK}, {Status: StatusWarn}}, 2},
		{"errors win", []Finding{{Status: StatusWarn}, {Status: StatusFail}}, 1},
		{"empty", nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Report{Findings: tc.findings}
			if got := r.ExitCode(); got != tc.want {
				t.Errorf("ExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSummary(t *testing.T) {
	tests := []struct {
		findings []Finding
		want     string
	}{
		{[]Finding{{Status: StatusOK}}, "all checks passed"},
		{[]Finding{{Status: StatusFail}}, "1 error"},
		{[]Finding{{Status: StatusFail}, {Status: StatusFail}, {Status: StatusWarn}}, "2 errors, 1 warning"},
		{[]Finding{{Status: StatusWarn}, {Status: StatusWarn}}, "2 warnings"},
	}
	for _, tc := range tests {
		r := &Report{Findings: tc.findings}
		if got := r.Summary(); got != tc.want {
			t.Errorf("Summary = %q, want %q", got, tc.want)
		}
	}
}

// Report order should read top-down: what's wrong with the config, then each
// host in the order the operator wrote them, then the edge.
func TestGroupedOrdering(t *testing.T) {
	r := &Report{Findings: []Finding{
		{Scope: ScopeEdge, Title: "api.example.com"},
		{Scope: ScopeHost, Host: "box-1", Title: "reachable"},
		{Scope: ScopeConfig, Title: "fleet.yaml valid"},
		{Scope: ScopeHost, Host: "web-1", Title: "reachable"},
		{Scope: ScopeHost, Host: "web-1", Title: "docker available"},
	}}

	groups := r.Grouped([]string{"web-1", "box-1"})
	if len(groups) != 4 {
		t.Fatalf("got %d groups, want 4", len(groups))
	}

	want := []struct {
		scope Scope
		host  string
	}{
		{ScopeConfig, ""},
		{ScopeHost, "web-1"},
		{ScopeHost, "box-1"},
		{ScopeEdge, ""},
	}
	for i, w := range want {
		if groups[i].Scope != w.scope || groups[i].Host != w.host {
			t.Errorf("group %d = %s/%s, want %s/%s", i, groups[i].Scope, groups[i].Host, w.scope, w.host)
		}
	}
	// Findings within a host keep the order they were produced in.
	if len(groups[1].Findings) != 2 || groups[1].Findings[0].Title != "reachable" {
		t.Errorf("web-1 findings = %+v", groups[1].Findings)
	}
}

func TestOfflineSkipsNetworkChecks(t *testing.T) {
	ran := map[string]bool{}
	checks := []Check{
		{Name: "local", Scope: ScopeConfig, Run: func(ctx context.Context, e *Env) []Finding {
			ran["local"] = true
			return []Finding{{Status: StatusOK, Title: "local"}}
		}},
		{Name: "remote", Scope: ScopeHost, NeedsNetwork: true, Run: func(ctx context.Context, e *Env) []Finding {
			ran["remote"] = true
			return nil
		}},
	}

	r := Run(context.Background(), &Env{Offline: true}, checks)
	if !ran["local"] {
		t.Error("offline mode should still validate config")
	}
	if ran["remote"] {
		t.Error("offline mode must not touch the network")
	}
	if len(r.Findings) != 1 {
		t.Errorf("got %d findings", len(r.Findings))
	}
}

func TestFixableCollectsRepairs(t *testing.T) {
	called := 0
	r := &Report{Findings: []Finding{
		{Status: StatusOK, Title: "fine"},
		{Status: StatusFail, Title: "broken", FixDesc: "fix it", Fix: func(context.Context) error {
			called++
			return nil
		}},
		{Status: StatusWarn, Title: "no auto-fix"},
	}}

	fixes := r.Fixable()
	if len(fixes) != 1 {
		t.Fatalf("got %d fixable findings, want 1", len(fixes))
	}
	if err := fixes[0].Fix(context.Background()); err != nil || called != 1 {
		t.Errorf("fix not invoked: %v", err)
	}
}

func TestFixErrorsPropagate(t *testing.T) {
	want := errors.New("caddy said no")
	f := Finding{Fix: func(context.Context) error { return want }}
	if err := f.Fix(context.Background()); !errors.Is(err, want) {
		t.Errorf("got %v", err)
	}
}

// The validator already did the work; checkConfig only translates severities.
func TestCheckConfigTranslatesDiagnostics(t *testing.T) {
	env := &Env{
		Fleet: &config.Fleet{
			Hosts:    map[string]*config.Host{"web-1": {Name: "web-1"}},
			Services: map[string]*config.Service{"api": {Name: "api"}},
		},
		Diags: config.Diagnostics{
			{Severity: config.SevError, File: "services/api.yaml", Field: "expose.upstream", Message: "out of range"},
			{Severity: config.SevWarning, File: "services/blog.yaml", Field: "health", Message: "no health check defined"},
		},
	}

	got := checkConfig(context.Background(), env)
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2 (no OK line when there are errors)", len(got))
	}
	if got[0].Status != StatusFail || !strings.Contains(got[0].Title, "expose.upstream") {
		t.Errorf("first finding = %+v", got[0])
	}
	if got[1].Status != StatusWarn {
		t.Errorf("warning not translated: %+v", got[1])
	}
}

func TestCheckConfigReportsCleanFleet(t *testing.T) {
	env := &Env{
		Fleet: &config.Fleet{
			Hosts:    map[string]*config.Host{"web-1": {}, "box-1": {}},
			Services: map[string]*config.Service{"api": {}},
		},
	}
	got := checkConfig(context.Background(), env)
	if len(got) != 1 || got[0].Status != StatusOK {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got[0].Title, "2 hosts") || !strings.Contains(got[0].Title, "1 service") {
		t.Errorf("summary should count hosts and services, got %q", got[0].Title)
	}
}

func TestParseDiskUsed(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want int
		ok   bool
	}{
		{
			"normal df output",
			"Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 41251136 13320520 25811560 34% /",
			34, true,
		},
		{
			"nearly full",
			"Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100 95 5 95% /opt",
			95, true,
		},
		{"header only", "Filesystem 1024-blocks Used Available Capacity Mounted on", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseDiskUsed(tc.out)
			if ok != tc.ok || got != tc.want {
				t.Errorf("parseDiskUsed = %d, %v; want %d, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestServicesOn(t *testing.T) {
	env := &Env{Fleet: &config.Fleet{
		Hosts: map[string]*config.Host{"web-1": {Name: "web-1"}, "box-1": {Name: "box-1"}},
		Services: map[string]*config.Service{
			"api":  {Name: "api", Hosts: []string{"web-1"}},
			"blog": {Name: "blog", Hosts: []string{"web-1"}},
			"job":  {Name: "job", Hosts: []string{"box-1"}},
			"both": {Name: "both", Hosts: []string{"web-1", "box-1"}},
		},
	}}

	var names []string
	for _, s := range env.ServicesOn("web-1") {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "api,blog,both" {
		t.Errorf("ServicesOn(web-1) = %v, want sorted api,blog,both", names)
	}
}

func TestStatusSymbols(t *testing.T) {
	for _, s := range []Status{StatusOK, StatusWarn, StatusFail, StatusSkipped} {
		if s.Symbol() == "?" || s.String() == "unknown" {
			t.Errorf("status %d has no symbol or name", s)
		}
	}
}
