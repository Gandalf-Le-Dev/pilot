// Command demo prints the README's terminal captures.
//
// The screenshots in the README used to be taken against the real fleet this
// project deploys itself with, which put one operator's hostnames and domains
// on the project's front page. This program renders the same output through
// Pilot's actual terminal renderer — the render package the CLI itself uses —
// fed with the example fleet's vocabulary, so a capture is pixel-faithful to
// what Pilot prints without photographing anyone's infrastructure.
//
// The vhs tapes alongside alias `pilot` to `go run ./docs/readme/demo`, so the
// typed command in each capture reads naturally. Being part of the module,
// this breaks the build the moment a renderer's signature changes — a capture
// can go stale, but it cannot quietly start lying about shapes.
package main

import (
	"fmt"
	"os"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/deploy"
	"github.com/Gandalf-Le-Dev/pilot/internal/doctor"
	"github.com/Gandalf-Le-Dev/pilot/internal/render"
)

func main() {
	// Extra arguments are accepted and ignored so the tapes can type the
	// command an operator would actually run — `pilot deploy api`.
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: demo status|doctor|deploy [args ignored]")
		os.Exit(2)
	}

	w := os.Stdout
	fmt.Fprintln(w)

	switch os.Args[1] {
	case "status":
		render.Status(w, statusRows())
	case "doctor":
		render.Doctor(w, doctorReport(), []string{"web-1", "web-2", "box-1"})
	case "deploy":
		printDeploy()
	default:
		fmt.Fprintf(os.Stderr, "unknown capture %q\n", os.Args[1])
		os.Exit(2)
	}

	fmt.Fprintln(w)
}

func statusRows() []render.StatusRow {
	return []render.StatusRow{
		{Service: "api", Host: "web-1", State: "running", Release: "0042-9f3ac1b"},
		{Service: "api", Host: "web-2", State: "running", Release: "0042-9f3ac1b"},
		{Service: "backup", Host: "web-1", State: "running", Release: "0019-8eec9e0", Detail: "next Fri 00:23 UTC"},
		{Service: "db", Host: "web-1", State: "running", Release: "0007-1fe22a1", Detail: "observe only"},
		{Service: "objects", Host: "box-1", State: "running", Release: "0031-4c00e8d"},
		{Service: "site", Host: "web-1", State: "running", Release: "0040-b52ffa1"},
		{Service: "site", Host: "web-2", State: "running", Release: "0040-b52ffa1"},
	}
}

func doctorReport() *doctor.Report {
	ok := func(scope doctor.Scope, host, title, detail string) doctor.Finding {
		return doctor.Finding{Status: doctor.StatusOK, Scope: scope, Host: host, Title: title, Detail: detail}
	}
	return &doctor.Report{Findings: []doctor.Finding{
		ok(doctor.ScopeConfig, "", "fleet.yaml valid (3 hosts, 5 services)", ""),

		ok(doctor.ScopeHost, "web-1", "docker available", ""),
		ok(doctor.ScopeHost, "web-1", "agent 0.11.2 (protocol 8)", ""),
		ok(doctor.ScopeHost, "web-1", "/etc/caddy/Caddyfile imports pilot.d/*.caddy", ""),
		ok(doctor.ScopeHost, "web-1", "disk 34% used", ""),

		ok(doctor.ScopeEdge, "", "api.example.com", "→ web-1, web-2  DNS ok  TLS valid 71d"),
		ok(doctor.ScopeEdge, "", "example.com", "→ web-1, web-2  DNS ok  TLS valid 84d"),
		{Status: doctor.StatusWarn, Scope: doctor.ScopeEdge, Title: "www.example.com",
			Detail: "→ web-1, web-2  DNS ok  TLS valid 9d (renewal window)"},
		ok(doctor.ScopeEdge, "", "objects.example.com", "→ box-1  DNS ok  TLS valid 89d"),
	}}
}

func printDeploy() {
	w := os.Stdout
	// "my-app" rather than "api": a first-time reader doesn't yet know
	// whether "api" is theirs or Pilot's, and the hero image is where they
	// meet the tool.
	svc := &config.Service{
		Name:    "my-app",
		Runtime: config.RuntimeCompose,
		Hosts:   []string{"web-1"},
		Expose:  &config.Expose{Domains: []string{"my-app.example.com"}, Upstream: 8080},
	}
	plan := &deploy.Plan{
		Service: svc,
		Release: "0042-9f3ac1b",
		Commit:  "b4a19c07d21f",
		Hosts: []deploy.HostPlan{{
			Host: "web-1", From: "0041-c96fe11", To: "0042-9f3ac1b",
			Route: deploy.RouteNone,
			Changes: []deploy.Change{
				{Field: "compose.yaml", From: "9f217c334ab1", To: "5d90f1e02c77"},
			},
		}},
	}

	fmt.Fprintln(w, "building my-app")
	fmt.Fprintln(w)
	render.Plan(w, plan)
	fmt.Fprintln(w)

	// The event stream a deploy prints while the host's agent works the job.
	// Mirrored from the phases the agent reports; if the phrasing changes,
	// re-run a real deploy and update these lines to match.
	for _, line := range []string{
		"staging 0042-9f3ac1b",
		"job j7-my-app running on the host",
		"activating 0042-9f3ac1b",
		"verifying health",
		"healthy",
		"recording state",
		"pruned 1 old release(s)",
	} {
		fmt.Fprintf(w, "  web-1: %s\n", line)
	}
	fmt.Fprintln(w)

	render.Outcomes(w, []deploy.Outcome{{Service: "my-app", Host: "web-1", Succeeded: true, Release: "0042-9f3ac1b"}})
}
