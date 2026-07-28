package compose

import (
	"strings"
	"testing"

	"github.com/gandalfledev/pilot/internal/config"
	"github.com/gandalfledev/pilot/internal/release"
	"github.com/gandalfledev/pilot/internal/runtime"
)

// Compose has changed the shape of `ps --format json` between minor versions:
// some emit a JSON array, some emit one object per line. Pilot accepts both
// rather than pinning users to a particular Docker release.
func TestParsePSAcceptsBothFormats(t *testing.T) {
	array := `[
	  {"Name":"api-web-1","Service":"web","Image":"ghcr.io/me/api@sha256:abc","State":"running","Health":"healthy","Status":"Up 2 hours"},
	  {"Name":"api-worker-1","Service":"worker","Image":"ghcr.io/me/api@sha256:abc","State":"running","Status":"Up 2 hours"}
	]`
	lines := `{"Name":"api-web-1","Service":"web","Image":"ghcr.io/me/api@sha256:abc","State":"running","Health":"healthy","Status":"Up 2 hours"}
{"Name":"api-worker-1","Service":"worker","Image":"ghcr.io/me/api@sha256:abc","State":"running","Status":"Up 2 hours"}`

	for name, raw := range map[string]string{"array": array, "json lines": lines} {
		t.Run(name, func(t *testing.T) {
			got, err := ParsePS([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("got %d instances, want 2", len(got))
			}
			if got[0].Name != "api-web-1" || got[0].State != "running" || got[0].Health != "healthy" {
				t.Errorf("instance = %+v", got[0])
			}
			if got[1].Image != "ghcr.io/me/api@sha256:abc" {
				t.Errorf("image lost: %+v", got[1])
			}
		})
	}
}

func TestParsePSEmpty(t *testing.T) {
	for _, raw := range []string{"", "  \n ", "[]"} {
		got, err := ParsePS([]byte(raw))
		if err != nil {
			t.Fatalf("ParsePS(%q): %v", raw, err)
		}
		if len(got) != 0 {
			t.Errorf("ParsePS(%q) = %v, want none", raw, got)
		}
	}
}

// Older compose omits State and only gives the human Status string.
func TestParsePSRecoversStateFromStatus(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{"Up 2 hours", "running"},
		{"Up 5 minutes (healthy)", "running"},
		{"Exited (1) 3 minutes ago", "exited"},
		{"Restarting (1) 2 seconds ago", "restarting"},
		{"Created", "created"},
		{"Paused", "paused"},
		{"something else", "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			got, err := ParsePS([]byte(`{"Name":"x","Status":"` + tc.status + `"}`))
			if err != nil {
				t.Fatal(err)
			}
			if got[0].State != tc.want {
				t.Errorf("status %q became state %q, want %q", tc.status, got[0].State, tc.want)
			}
		})
	}
}

func TestParsePSRecordsExitCode(t *testing.T) {
	got, err := ParsePS([]byte(`{"Name":"api-web-1","State":"exited","Status":"Exited (137)","ExitCode":137}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got[0].Detail, "137") {
		t.Errorf("exit code should reach the detail line, got %q", got[0].Detail)
	}
}

func TestParsePSRejectsGarbage(t *testing.T) {
	if _, err := ParsePS([]byte("not json at all")); err == nil {
		t.Error("want an error")
	}
}

func TestSummarise(t *testing.T) {
	inst := func(state, health string) runtime.Instance {
		return runtime.Instance{Name: "c", State: state, Health: health}
	}

	tests := []struct {
		name      string
		current   string
		instances []runtime.Instance
		want      runtime.State
	}{
		{"nothing deployed", "", nil, runtime.StateStopped},
		{"no containers", "0042-abc1234", nil, runtime.StateStopped},
		{
			"all running",
			"0042-abc1234",
			[]runtime.Instance{inst("running", "healthy"), inst("running", "")},
			runtime.StateRunning,
		},
		{
			"one unhealthy",
			"0042-abc1234",
			[]runtime.Instance{inst("running", "healthy"), inst("running", "unhealthy")},
			runtime.StateDegraded,
		},
		{
			"restart loop",
			"0042-abc1234",
			[]runtime.Instance{inst("restarting", "")},
			runtime.StateDegraded,
		},
		{
			"partially down",
			"0042-abc1234",
			[]runtime.Instance{inst("running", ""), inst("exited", "")},
			runtime.StateDegraded,
		},
		{
			"all stopped",
			"0042-abc1234",
			[]runtime.Instance{inst("exited", ""), inst("exited", "")},
			runtime.StateStopped,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := summarise(tc.instances, tc.current)
			if got != tc.want {
				t.Errorf("summarise = %q (%s), want %q", got, detail, tc.want)
			}
			if got != runtime.StateRunning && detail == "" {
				t.Error("a non-running verdict should explain itself")
			}
		})
	}
}

// The pinned project name is what makes a deploy an update rather than a
// second copy of the stack.
func TestComposeCommandPinsProject(t *testing.T) {
	got := cmd("/opt/pilot/services/api/current", "api", "up", "--detach")

	for _, want := range []string{
		"--project-name api",
		"--project-directory /opt/pilot/services/api/current",
		"--file /opt/pilot/services/api/current/compose.yaml",
		"--env-file /opt/pilot/services/api/current/.env",
		"up --detach",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
}

func TestComposeCommandQuotesUnsafeNames(t *testing.T) {
	got := cmd("/opt/pilot/services/my app/current", "my app", "up")
	if !strings.Contains(got, "'my app'") {
		t.Errorf("project name with a space must be quoted: %s", got)
	}
}

func TestProjectDefaultsToServiceName(t *testing.T) {
	tg := &runtime.Target{Service: &config.Service{Name: "api"}, Layout: release.NewLayout("")}
	if got := project(tg); got != "api" {
		t.Errorf("project = %q, want the service name", got)
	}

	tg.Service.Compose = &config.Compose{Project: "legacy-api"}
	if got := project(tg); got != "legacy-api" {
		t.Errorf("project = %q, want the configured override", got)
	}
}

func TestKind(t *testing.T) {
	if New().Kind() != config.RuntimeCompose {
		t.Error("wrong runtime kind")
	}
}
