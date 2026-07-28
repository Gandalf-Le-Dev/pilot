package deploy

import (
	"strings"
	"testing"
	"time"

	"github.com/gandalfledev/pilot/internal/config"
	"github.com/gandalfledev/pilot/internal/transport/proto"
)

// The spec the CLI sends is the spec the agent will monitor with. If it does
// not round-trip, the host ends up unable to observe, probe, or alert on the
// very service it just deployed — and would only find out at 3am.
func TestSpecForRoundTrips(t *testing.T) {
	tests := []struct {
		name string
		svc  *config.Service
	}{
		{
			"compose with a route and health check",
			&config.Service{
				Name: "api", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
				Manage: config.ManageDeploy, KeepReleases: 5,
				Compose: &config.Compose{File: "deploy/compose.yaml", Project: "api"},
				Env:     map[string]string{"LOG_LEVEL": "info"},
				Expose: &config.Expose{
					Domains: []string{"api.example.com"}, Upstream: 8080, Verify: true,
				},
				Health: &config.Health{
					HTTP:    &config.HTTPProbe{URL: "http://localhost:8080/healthz", Expect: 200},
					Timeout: config.Duration(90 * time.Second),
				},
				Rollout: &config.Rollout{Strategy: config.StrategyRecreate, Concurrency: 1},
				Alerts: []config.Alert{
					{When: "service.down", For: config.Duration(2 * time.Minute), Notify: []string{"ntfy"}},
				},
			},
		},
		{
			"static site",
			&config.Service{
				Name: "blog", Runtime: config.RuntimeStatic, Hosts: []string{"web-1"},
				Manage: config.ManageDeploy, KeepReleases: 5,
				Build: &config.Build{Command: "npm run build", Output: []string{"dist/"}},
				Expose: &config.Expose{
					Domains: []string{"blog.example.com"},
					Static:  &config.StaticExpose{SPA: true, Index: "index.html", Overlay: []string{"assets"}},
				},
				Rollout: &config.Rollout{Strategy: config.StrategyRecreate, Concurrency: 1},
			},
		},
		{
			"blue-green",
			&config.Service{
				Name: "api", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
				Manage: config.ManageDeploy, KeepReleases: 5,
				Compose: &config.Compose{File: "compose.yaml", Project: "api"},
				Expose:  &config.Expose{Domains: []string{"api.example.com"}, Upstream: 8080},
				Health:  &config.Health{Docker: true},
				Rollout: &config.Rollout{
					Strategy: config.StrategyBlueGreen, Service: "web",
					Ports: []int{18080, 18081}, Drain: config.Duration(30 * time.Second),
					Concurrency: 1,
				},
			},
		},
		{
			"observe only",
			&config.Service{
				Name: "postgres", Runtime: config.RuntimeCompose, Hosts: []string{"box-1"},
				Manage: config.ManageObserve, KeepReleases: 5,
				Health:  &config.Health{Exec: &config.ExecProbe{Cmd: []string{"pg_isready"}}},
				Rollout: &config.Rollout{Strategy: config.StrategyRecreate, Concurrency: 1},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := SpecFor(tc.svc)
			if err != nil {
				t.Fatalf("SpecFor: %v", err)
			}

			got, err := config.ParseService([]byte(spec), tc.svc.Name+".yaml")
			if err != nil {
				t.Fatalf("the agent could not parse the spec we would send it: %v\n%s", err, spec)
			}

			if got.Name != tc.svc.Name || got.Runtime != tc.svc.Runtime || got.Manage != tc.svc.Manage {
				t.Errorf("identity lost: %+v", got)
			}

			// The details the agent actually needs to keep working.
			if tc.svc.Health != nil && got.Health.Probes() != tc.svc.Health.Probes() {
				t.Errorf("health check lost: %+v", got.Health)
			}
			if tc.svc.Expose != nil {
				if got.Expose == nil || len(got.Expose.Domains) != len(tc.svc.Expose.Domains) {
					t.Errorf("route lost: %+v", got.Expose)
				}
			}
			if len(got.Alerts) != len(tc.svc.Alerts) {
				t.Errorf("alert rules lost: %+v", got.Alerts)
			}
			if tc.svc.Rollout.IsBlueGreen() {
				if !got.Rollout.IsBlueGreen() || got.Rollout.PortFor(config.ColorGreen) != 18081 {
					t.Errorf("blue-green settings lost: %+v", got.Rollout)
				}
			}
		})
	}
}

// A duration must survive as a duration, not as a nanosecond count nobody can
// read in the cached file.
func TestSpecPreservesDurations(t *testing.T) {
	svc := &config.Service{
		Name: "api", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Manage: config.ManageDeploy, KeepReleases: 5,
		Compose: &config.Compose{File: "c.yaml", Project: "api"},
		Health:  &config.Health{Docker: true, Timeout: config.Duration(90 * time.Second)},
		Rollout: &config.Rollout{Strategy: config.StrategyRecreate, Concurrency: 1},
	}

	spec, err := SpecFor(svc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec, "1m30s") && !strings.Contains(spec, "90s") {
		t.Errorf("duration should be human-readable in the cached spec:\n%s", spec)
	}

	got, err := config.ParseService([]byte(spec), "api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got.Health.Timeout.Duration() != 90*time.Second {
		t.Errorf("timeout = %s, want 90s", got.Health.Timeout)
	}
}

// Env values are sent so the agent can render .env, but a spec is written to
// the host's cache — it must not be the thing that leaks a secret into a place
// nobody expects.
func TestSpecCarriesEnvKeysAndValues(t *testing.T) {
	svc := &config.Service{
		Name: "api", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Manage: config.ManageDeploy, KeepReleases: 5,
		Compose: &config.Compose{File: "c.yaml", Project: "api"},
		Env:     map[string]string{"LOG_LEVEL": "info"},
		Health:  &config.Health{Docker: true},
		Rollout: &config.Rollout{Strategy: config.StrategyRecreate, Concurrency: 1},
	}

	spec, err := SpecFor(svc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.ParseService([]byte(spec), "api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got.Env["LOG_LEVEL"] != "info" {
		t.Errorf("env lost: %+v", got.Env)
	}
}

func TestToProtoRoute(t *testing.T) {
	tests := []struct {
		in   RouteAction
		want proto.RouteAction
	}{
		{RouteInstall, proto.RouteInstall},
		{RouteUpdate, proto.RouteUpdate},
		{RouteNone, proto.RouteNone},
		{RouteNotExposed, ""},
	}
	for _, tc := range tests {
		if got := toProtoRoute(tc.in); got != tc.want {
			t.Errorf("toProtoRoute(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
