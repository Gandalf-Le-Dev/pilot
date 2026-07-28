package compose

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// recorder captures the commands a runtime would run, and serves a state.json
// so colour resolution has something to read.
type recorder struct {
	cmds  []string
	files map[string]string
	psOut string
}

func (r *recorder) Run(ctx context.Context, cmd string) (transport.Result, error) {
	r.cmds = append(r.cmds, cmd)
	switch {
	case strings.Contains(cmd, "ps --all"):
		return transport.Result{Stdout: r.psOut}, nil
	case strings.HasPrefix(cmd, "readlink"):
		return transport.Result{Stdout: "releases/0001-aaaaaaa\n"}, nil
	case strings.HasPrefix(cmd, "cat "):
		for p, v := range r.files {
			if strings.Contains(cmd, p) {
				return transport.Result{Stdout: v}, nil
			}
		}
		return transport.Result{Stdout: ""}, nil
	}
	return transport.Result{}, nil
}

func (r *recorder) RunScript(ctx context.Context, b string) (transport.Result, error) {
	return r.Run(ctx, b)
}
func (r *recorder) RunInput(ctx context.Context, c string, _ []byte) (transport.Result, error) {
	return r.Run(ctx, c)
}
func (r *recorder) Stream(ctx context.Context, c string, _, _ io.Writer) (int, error) {
	res, _ := r.Run(ctx, c)
	return res.ExitCode, nil
}
func (r *recorder) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if v, ok := r.files[p]; ok {
		return []byte(v), nil
	}
	return nil, os.ErrNotExist
}
func (r *recorder) WriteFile(ctx context.Context, p string, d []byte, m string) error {
	if r.files == nil {
		r.files = map[string]string{}
	}
	r.files[p] = string(d)
	return nil
}
func (r *recorder) Exists(ctx context.Context, p string) (bool, error)     { return true, nil }
func (r *recorder) HasCommand(ctx context.Context, n string) (bool, error) { return true, nil }
func (r *recorder) UploadDir(ctx context.Context, a, b string) error       { return nil }
func (r *recorder) MkdirAll(ctx context.Context, d string) error           { return nil }
func (r *recorder) RemoveAll(ctx context.Context, p string) error          { return nil }
func (r *recorder) Label() string                                          { return "web-1" }

func (r *recorder) ran(substr string) bool {
	for _, c := range r.cmds {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func bgTarget(t *testing.T, activeColor string) (*runtime.Target, *recorder) {
	t.Helper()

	svc := &config.Service{
		Name: "web", Runtime: config.RuntimeCompose,
		Compose: &config.Compose{Project: "web"},
		Expose:  &config.Expose{Domains: []string{"web.example.com"}, Upstream: 80},
		Rollout: &config.Rollout{
			Strategy: config.StrategyBlueGreen, Service: "web", Ports: []int{18080, 18081},
		},
	}
	layout := release.NewLayout("")

	st := release.NewState("web")
	st.Current = "0001-aaaaaaa"
	st.ActiveColor = activeColor
	body, err := release.MarshalState(st)
	if err != nil {
		t.Fatal(err)
	}

	rec := &recorder{
		files: map[string]string{layout.State("web"): string(body)},
		psOut: `[{"Name":"web-green-web-1","State":"running","Image":"nginx:alpine"}]`,
	}
	return &runtime.Target{
		Service: svc,
		Host:    &config.Host{Name: "web-1"},
		Layout:  layout,
		Client:  rec,
	}, rec
}

// Under blue-green the containers live in `<svc>-green`, never in `<svc>`.
// Asking about the wrong project reports a running service as stopped — which
// is exactly what happened before this was fixed.
func TestActiveProjectFollowsTheLiveColour(t *testing.T) {
	for _, colour := range []string{"blue", "green"} {
		t.Run(colour, func(t *testing.T) {
			tg, _ := bgTarget(t, colour)
			if got := ActiveProject(context.Background(), tg); got != "web-"+colour {
				t.Errorf("ActiveProject = %q, want web-%s", got, colour)
			}
		})
	}
}

func TestActiveProjectIsPlainForRecreateServices(t *testing.T) {
	tg, _ := bgTarget(t, "green")
	tg.Service.Rollout = &config.Rollout{Strategy: config.StrategyRecreate}

	if got := ActiveProject(context.Background(), tg); got != "web" {
		t.Errorf("ActiveProject = %q, want the plain project name", got)
	}
}

// No recorded colour means nothing has been deployed yet; blue is assumed so
// the first deploy targets green, matching the agent.
func TestActiveColourDefaultsToBlue(t *testing.T) {
	tg, rec := bgTarget(t, "")
	rec.files = map[string]string{}

	if got := ActiveColor(context.Background(), tg); got != config.ColorBlue {
		t.Errorf("ActiveColor = %q, want blue", got)
	}
}

func TestObserveQueriesTheLiveColour(t *testing.T) {
	tg, rec := bgTarget(t, "green")

	obs, err := New().Observe(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.ran("--project-name web-green") {
		t.Errorf("Observe asked the wrong project:\n%v", rec.cmds)
	}
	if obs.State != runtime.StateRunning {
		t.Errorf("state = %q, want running — this is the bug where a live service reads as stopped", obs.State)
	}
}

func TestLogsComeFromTheLiveColour(t *testing.T) {
	tg, rec := bgTarget(t, "green")

	if err := New().Logs(context.Background(), tg, runtime.LogOptions{Tail: 10}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !rec.ran("--project-name web-green logs") {
		t.Errorf("logs asked the wrong project:\n%v", rec.cmds)
	}
}

func TestFingerprintUsesTheLiveColour(t *testing.T) {
	tg, rec := bgTarget(t, "blue")

	if _, err := New().Fingerprint(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if !rec.ran("compose.pilot-blue.yaml") {
		t.Errorf("fingerprint should resolve the live colour's override:\n%v", rec.cmds)
	}
}

// A plain `up -d` would create a third, unrouted stack beside the two real
// ones. Refusing is the honest failure.
func TestActivateRefusesBlueGreen(t *testing.T) {
	tg, _ := bgTarget(t, "green")
	tg.Release = "0002-bbbbbbb"

	err := New().Activate(context.Background(), tg)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "activated by the agent") {
		t.Errorf("error should explain: %v", err)
	}
	if !strings.Contains(err.Error(), "pilot bootstrap") {
		t.Errorf("error should say how to fix it: %v", err)
	}
}

func TestDeactivateStopsTheLiveColour(t *testing.T) {
	tg, rec := bgTarget(t, "green")

	if err := New().Deactivate(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if !rec.ran("--project-name web-green") || !rec.ran("down") {
		t.Errorf("Deactivate asked the wrong project:\n%v", rec.cmds)
	}
}

// The override files are written for both colours at stage time, so either can
// be started later from the same release.
func TestStageColorWritesBothOverrides(t *testing.T) {
	tg, rec := bgTarget(t, "green")
	tg.Release = "0001-aaaaaaa"

	if err := StageColor(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	dir := tg.ReleaseDir()
	for colour, port := range map[string]string{"blue": "18080", "green": "18081"} {
		body, ok := rec.files[filepath.Join(dir, "compose.pilot-"+colour+".yaml")]
		if !ok {
			t.Fatalf("no override written for %s", colour)
		}
		if !strings.Contains(body, "127.0.0.1:"+port+":80") {
			t.Errorf("%s override has the wrong mapping:\n%s", colour, body)
		}
	}
}

func TestStageColorNeedsAContainerPort(t *testing.T) {
	tg, _ := bgTarget(t, "green")
	tg.Service.Expose = nil

	if err := StageColor(context.Background(), tg); err == nil {
		t.Error("blue-green needs expose.upstream to know which container port to publish")
	}
}

// Guards the assumption the recorder relies on, so a change to the state format
// fails here rather than silently making colour resolution wrong.
func TestStateRoundTripsActiveColour(t *testing.T) {
	st := release.NewState("web")
	st.ActiveColor = "green"

	body, err := release.MarshalState(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "active_color") {
		t.Errorf("colour not persisted:\n%s", body)
	}

	var back release.State
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if back.ActiveColor != "green" {
		t.Errorf("ActiveColor = %q", back.ActiveColor)
	}
}
