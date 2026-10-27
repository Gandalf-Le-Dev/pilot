package compose

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
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

// Docker writes `0001-01-01T00:00:00Z` for a time that never happened; it must
// read as no time at all, not as an age of two thousand years.
func TestParseInspectSince(t *testing.T) {
	started := time.Date(2026, 10, 7, 18, 0, 40, 748848514, time.UTC)
	finished := time.Date(2026, 10, 7, 18, 0, 37, 723703083, time.UTC)

	tests := []struct {
		name string
		line string
		want time.Time
	}{
		{
			"running uses its last start",
			`{"id":"a2c2","name":"/wakapi","running":true,"started":"2026-10-07T18:00:40.748848514Z","finished":"2026-10-07T18:00:37.723703083Z"}`,
			started,
		},
		{
			"stopped uses its last stop",
			`{"id":"a2c2","name":"/wakapi","running":false,"started":"2026-10-07T18:00:30Z","finished":"2026-10-07T18:00:37.723703083Z"}`,
			finished,
		},
		{
			"never started is zero",
			`{"id":"a2c2","name":"/wakapi","running":false,"started":"0001-01-01T00:00:00Z","finished":"0001-01-01T00:00:00Z"}`,
			time.Time{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseInspect([]byte(tc.line + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d entries, want 1", len(got))
			}
			if since := got[0].since(); !since.Equal(tc.want) {
				t.Errorf("since = %v, want %v", since, tc.want)
			}
		})
	}
}

func TestParseInspectRejectsGarbage(t *testing.T) {
	if _, err := parseInspect([]byte("Template parsing error")); err == nil {
		t.Error("want an error")
	}
}

// ps only reports CreatedAt, which survives restarts and reboots: a container
// created in August and restarted today would show months of age.
func TestObserveDatesContainersFromLastStart(t *testing.T) {
	tg, rec := bgTarget(t, "green")
	rec.psOut = `{"ID":"aaa111","Name":"web-green-web-1","State":"running","CreatedAt":"2026-08-12 10:00:00 +0000 UTC"}
{"ID":"bbb222","Name":"web-green-worker-1","State":"running","CreatedAt":"2026-08-12 10:00:00 +0000 UTC"}`
	rec.inspectOut = `{"id":"aaa111ffff","name":"/web-green-web-1","running":true,"started":"2026-10-07T18:00:40Z","finished":"0001-01-01T00:00:00Z"}
{"id":"bbb222ffff","name":"/web-green-worker-1","running":true,"started":"2026-10-07T18:00:45Z","finished":"0001-01-01T00:00:00Z"}
`

	obs, err := New().Observe(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}

	web := time.Date(2026, 10, 7, 18, 0, 40, 0, time.UTC)
	worker := time.Date(2026, 10, 7, 18, 0, 45, 0, time.UTC)
	if len(obs.Instances) != 2 {
		t.Fatalf("got %d instances, want 2", len(obs.Instances))
	}
	if !obs.Instances[0].Since.Equal(web) || !obs.Instances[1].Since.Equal(worker) {
		t.Errorf("since = %v, %v; want %v, %v", obs.Instances[0].Since, obs.Instances[1].Since, web, worker)
	}
	if !obs.Since.Equal(web) {
		t.Errorf("observation since = %v, want the earliest start %v", obs.Since, web)
	}

	var inspects int
	for _, c := range rec.cmds {
		if strings.HasPrefix(c, "docker inspect") {
			inspects++
			if !strings.Contains(c, "aaa111") || !strings.Contains(c, "bbb222") {
				t.Errorf("inspect should name every container by ID: %s", c)
			}
		}
	}
	if inspects != 1 {
		t.Errorf("ran %d inspects, want one per observation", inspects)
	}
}

// Older compose has no ID in ps; the container name still finds it, despite
// inspect reporting it with a leading slash.
func TestObserveDatesContainersByNameWithoutID(t *testing.T) {
	tg, rec := bgTarget(t, "green")
	rec.inspectOut = `{"id":"ccc333","name":"/web-green-web-1","running":true,"started":"2026-10-07T18:00:40Z","finished":"0001-01-01T00:00:00Z"}`

	obs, err := New().Observe(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 7, 18, 0, 40, 0, time.UTC); !obs.Instances[0].Since.Equal(want) {
		t.Errorf("since = %v, want %v", obs.Instances[0].Since, want)
	}
}

// An age is cosmetic; losing it must not turn a running service into a failed
// observation.
func TestObserveSurvivesFailedInspect(t *testing.T) {
	for name, set := range map[string]func(*recorder){
		"exit code":   func(r *recorder) { r.inspectFail = true },
		"unparseable": func(r *recorder) { r.inspectOut = "Template parsing error" },
	} {
		t.Run(name, func(t *testing.T) {
			tg, rec := bgTarget(t, "green")
			set(rec)

			obs, err := New().Observe(context.Background(), tg)
			if err != nil {
				t.Fatal(err)
			}
			if obs.State != runtime.StateRunning {
				t.Errorf("state = %q, want running", obs.State)
			}
			if !obs.Since.IsZero() || !obs.Instances[0].Since.IsZero() {
				t.Errorf("since should stay blank, got %v", obs.Since)
			}
		})
	}
}

func TestObserveColorDatesContainersFromLastStop(t *testing.T) {
	tg, rec := bgTarget(t, "green")
	rec.psOut = `[{"ID":"ddd444","Name":"web-blue-web-1","State":"exited","Status":"Exited (0)"}]`
	rec.inspectOut = `{"id":"ddd444ffff","name":"/web-blue-web-1","running":false,"started":"2026-10-01T09:00:00Z","finished":"2026-10-07T18:00:37Z"}`

	obs, err := ObserveColor(context.Background(), tg, config.ColorBlue)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 7, 18, 0, 37, 0, time.UTC); !obs.Instances[0].Since.Equal(want) {
		t.Errorf("since = %v, want the last stop %v", obs.Instances[0].Since, want)
	}
}
