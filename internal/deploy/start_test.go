package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/remote"
	"github.com/Gandalf-Le-Dev/pilot/internal/build"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/local"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// observer is a runtime whose Observe answers with a fixed result and whose
// Activate records which release it was asked to make live.
type observer struct {
	stubRuntime
	err       error
	activated *[]string
}

func (o observer) Observe(context.Context, *runtime.Target) (runtime.Observation, error) {
	return o.obs, o.err
}

func (o observer) Activate(_ context.Context, t *runtime.Target) error {
	if o.activated != nil {
		*o.activated = append(*o.activated, t.Release)
	}
	return nil
}

// liveInput returns plan input for a host that already runs exactly the release
// the input describes, with its route installed.
func liveInput(t *testing.T, s *config.Service) (Input, string) {
	t.Helper()
	root := t.TempDir()
	layout := release.NewLayout(filepath.Join(root, "pilot"))
	fleet := &config.Fleet{Caddy: config.Caddy{SnippetDir: filepath.Join(root, "pilot.d")}}
	target := &runtime.Target{
		Service: s, Host: &config.Host{Name: "web-1"}, Layout: layout, Client: local.New("web-1"),
	}

	in := Input{
		Service: s, Fleet: fleet, Layout: layout,
		Build:   &build.Result{Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaa"}}},
		Env:     map[string]string{"LOG_LEVEL": "info"},
		Targets: map[string]*runtime.Target{"web-1": target},
	}

	first, err := Compute(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	body, err := release.MarshalManifest(first.Manifest("web-1"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, layout.Manifest(s.Name, first.Release), string(body))
	write(t, path.Join(fleet.Caddy.SnippetDir, caddy.SnippetName(s.Name)), first.Snippet)

	live := *target
	live.Release = first.Release
	if err := runtime.Swap(t.Context(), &live); err != nil {
		t.Fatal(err)
	}
	return in, first.Release
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Seen after a VPS reboot: a compose service sat in Exited (128) because its
// port bound to a tailnet address that did not exist yet, and `pilot deploy`
// answered "nothing to do". A plan that is otherwise a no-op must start a
// stopped service, and only a stopped one.
func TestPlanStartsAStoppedService(t *testing.T) {
	compose := func() *config.Service {
		return &config.Service{
			Name: "kite", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
			Expose: &config.Expose{Domains: []string{"kite.example.com"}, Upstream: 8080},
		}
	}
	oneshot := func() *config.Service {
		s := compose()
		s.Runtime = config.RuntimeSystemd
		s.Unit = &config.Unit{Name: "backup.service", Kind: config.UnitOneshot, Timer: "backup.timer"}
		return s
	}
	static := func() *config.Service {
		s := compose()
		s.Runtime = config.RuntimeStatic
		s.Expose = &config.Expose{Domains: []string{"kite.example.com"}, Static: &config.StaticExpose{}}
		return s
	}
	blueGreen := func() *config.Service {
		s := compose()
		s.Rollout = &config.Rollout{Strategy: config.StrategyBlueGreen, Service: "web", Ports: []int{18080, 18081}}
		return s
	}

	exited := []runtime.Instance{{Name: "kite-web-1", State: "exited"}}
	stoppedWith := func(in []runtime.Instance) runtime.Observation {
		return runtime.Observation{State: runtime.StateStopped, Detail: "all containers stopped", Instances: in}
	}

	tests := []struct {
		name           string
		svc            func() *config.Service
		obs            runtime.Observation
		err            error
		agent          bool
		skewed         bool
		routeChanged   bool
		wantStart      bool
		wantNotStarted string
	}{
		{name: "exited containers", svc: compose, obs: stoppedWith(exited), wantStart: true},
		{name: "failed unit", svc: compose, obs: runtime.Observation{State: runtime.StateFailed, Instances: exited}, wantStart: true},
		{name: "running", svc: compose, obs: runtime.Observation{State: runtime.StateRunning, Instances: exited}},
		{name: "degraded is not stopped", svc: compose, obs: runtime.Observation{State: runtime.StateDegraded, Instances: exited}},
		{name: "unknown is not stopped", svc: compose, obs: runtime.Observation{State: runtime.StateUnknown}},
		{name: "observation failed", svc: compose, err: errors.New("ssh: broken pipe")},
		// A changed `rollout` makes the plan look in a project the live stack
		// is not in. Starting it would raise a second, unrouted stack.
		{name: "no containers at all", svc: compose, obs: stoppedWith(nil)},
		{name: "oneshot between timer runs", svc: oneshot, obs: runtime.Observation{State: runtime.StateStopped, AwaitingRun: true, Instances: exited}},
		{name: "oneshot whose last run failed", svc: oneshot, obs: runtime.Observation{State: runtime.StateFailed, Instances: exited}},
		{name: "static site has nothing to start", svc: static, obs: stoppedWith(exited)},
		{name: "blue-green with an agent", svc: blueGreen, obs: stoppedWith(exited), agent: true, wantStart: true},
		{name: "blue-green without an agent", svc: blueGreen, obs: stoppedWith(exited), wantNotStarted: "pilot bootstrap web-1"},
		{name: "blue-green with a skewed agent", svc: blueGreen, obs: stoppedWith(exited), skewed: true, wantNotStarted: "pilot agent upgrade web-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in, live := liveInput(t, tc.svc())
			in.Runtime = observer{stubRuntime: stubRuntime{tc.obs}, err: tc.err}
			if tc.agent {
				in.Agents = map[string]*remote.Client{"web-1": {}}
			}
			in.Skewed = map[string]bool{"web-1": tc.skewed}

			p, err := Compute(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			hp := p.Hosts[0]
			if hp.Start != tc.wantStart || hp.NoOp == tc.wantStart {
				t.Fatalf("start=%v noop=%v, want start=%v", hp.Start, hp.NoOp, tc.wantStart)
			}
			if p.NoOp() == tc.wantStart {
				t.Errorf("plan NoOp = %v with start=%v; a start is not nothing to do", p.NoOp(), tc.wantStart)
			}
			// Nothing new ships, so the live release is the one that stays.
			if hp.To != live || hp.From != live {
				t.Errorf("from %q to %q, want both %q", hp.From, hp.To, live)
			}
			if tc.wantStart && hp.Observed == "" {
				t.Error("a start should carry what the host reported")
			}
			if (hp.NotStarted == "") != (tc.wantNotStarted == "") || !strings.Contains(hp.NotStarted, tc.wantNotStarted) {
				t.Errorf("NotStarted = %q, want it to name %q", hp.NotStarted, tc.wantNotStarted)
			}
		})
	}
}

// A stopped host that also has something new to ship gets the whole deploy,
// with its stage and rollback, rather than a start of the old release.
func TestPlanDeploysAStoppedHostWithAChangedRoute(t *testing.T) {
	s := &config.Service{
		Name: "kite", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Expose: &config.Expose{Domains: []string{"kite.example.com"}, Upstream: 8080},
	}
	in, live := liveInput(t, s)
	in.Runtime = observer{stubRuntime: stubRuntime{runtime.Observation{
		State: runtime.StateStopped, Instances: []runtime.Instance{{Name: "kite-web-1"}},
	}}}
	write(t, path.Join(in.Fleet.Caddy.SnippetDir, caddy.SnippetName(s.Name)), "# edited by hand\n")

	p, err := Compute(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	hp := p.Hosts[0]
	if hp.Start || hp.NoOp || hp.Route != RouteUpdate {
		t.Fatalf("start=%v noop=%v route=%q, want a deploy that updates the route", hp.Start, hp.NoOp, hp.Route)
	}
	if hp.To == live || hp.To != p.Release {
		t.Errorf("to = %q, want the new release %q", hp.To, p.Release)
	}
}

func TestPlanSaysWhenAStartHasNoHealthGate(t *testing.T) {
	s := &config.Service{
		Name: "kite", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Expose: &config.Expose{Domains: []string{"kite.example.com"}, Upstream: 8080},
	}
	in, _ := liveInput(t, s)
	in.Runtime = observer{stubRuntime: stubRuntime{runtime.Observation{
		State: runtime.StateStopped, Instances: []runtime.Instance{{Name: "kite-web-1"}},
	}}}

	tests := []struct {
		name       string
		health     *config.Health
		skipVerify bool
		want       bool
	}{
		{"health check", &config.Health{Docker: true}, false, false},
		{"no health check", nil, false, true},
		{"--no-verify", &config.Health{Docker: true}, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s.Health = tc.health
			in.SkipVerify = tc.skipVerify
			p, err := Compute(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			if p.NoHealthGate != tc.want {
				t.Errorf("NoHealthGate = %v, want %v", p.NoHealthGate, tc.want)
			}
		})
	}
}

// An observe-only service is never deployed, and so never started either.
func TestPlanRefusesToStartAnObservedService(t *testing.T) {
	s := &config.Service{Name: "postgres", Runtime: config.RuntimeCompose, Manage: config.ManageObserve}
	if _, err := Compute(t.Context(), Input{Service: s}); err == nil {
		t.Error("planning a `manage: observe` service should be refused")
	}
}

// startExecutor returns an executor and a plan that starts a stopped service on
// a host whose live release is 0002, with 0001 as its rollback target.
func startExecutor(t *testing.T, obs runtime.State, activated *[]string) (*Executor, *Plan, *runtime.Target) {
	t.Helper()
	s := &config.Service{
		Name: "kite", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Health: &config.Health{
			Docker:  true,
			Timeout: config.Duration(time.Second), Interval: config.Duration(300 * time.Millisecond),
		},
	}
	target := &runtime.Target{
		Service: s, Host: &config.Host{Name: "web-1"},
		Layout: release.NewLayout(t.TempDir()), Client: local.New("web-1"),
	}
	for _, id := range []string{"0001-aaaaaaa", "0002-bbbbbbb"} {
		write(t, target.Layout.Manifest(s.Name, id), "{}")
	}
	st := release.NewState(s.Name)
	st.Current, st.Previous = "0002-bbbbbbb", "0001-aaaaaaa"
	if err := runtime.WriteState(t.Context(), target, st); err != nil {
		t.Fatal(err)
	}
	live := *target
	live.Release = st.Current
	if err := runtime.Swap(t.Context(), &live); err != nil {
		t.Fatal(err)
	}

	e := &Executor{
		Runtime: observer{stubRuntime: stubRuntime{runtime.Observation{State: obs}}, activated: activated},
		Targets: map[string]*runtime.Target{"web-1": target},
		Log:     func(string, ...any) {},
	}
	p := &Plan{
		Service: s, Release: "0003-bbbbbbb",
		Hosts: []HostPlan{{
			Host: "web-1", From: "0002-bbbbbbb", To: "0002-bbbbbbb",
			Start: true, Observed: "stopped (all containers stopped)",
		}},
	}
	return e, p, target
}

// There is no earlier release worth restoring: the live one was healthy until
// the host rebooted. A failed start is reported and the host left on it.
func TestStartNeverRollsBack(t *testing.T) {
	var activated []string
	e, p, target := startExecutor(t, runtime.StateStopped, &activated)

	outcomes, err := e.Run(t.Context(), p)
	if err == nil {
		t.Fatal("a start that fails its health check must fail the deploy")
	}
	out := outcomes[0]
	if out.Succeeded || out.RolledBack {
		t.Errorf("succeeded=%v rolledBack=%v", out.Succeeded, out.RolledBack)
	}
	if len(activated) != 1 || activated[0] != "0002-bbbbbbb" {
		t.Errorf("activated %v, want only the live release, once", activated)
	}

	st, err := runtime.ReadState(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "0002-bbbbbbb" || st.Previous != "0001-aaaaaaa" {
		t.Errorf("state = current %q previous %q, want both unchanged", st.Current, st.Previous)
	}
	if last := st.LastDeploy(); last == nil || last.Outcome != release.OutcomeFailed || last.Release != "0002-bbbbbbb" {
		t.Errorf("history should record the failed start: %+v", last)
	}
}

func TestStartRecordsAStartNotADeploy(t *testing.T) {
	var activated []string
	e, p, target := startExecutor(t, runtime.StateRunning, &activated)

	outcomes, err := e.Run(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if out := outcomes[0]; !out.Succeeded || out.Message != "started" || out.Release != "0002-bbbbbbb" {
		t.Errorf("outcome = %+v", out)
	}

	st, err := runtime.ReadState(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "0002-bbbbbbb" || st.Previous != "0001-aaaaaaa" {
		t.Errorf("state = current %q previous %q, want both unchanged", st.Current, st.Previous)
	}
	if last := st.LastDeploy(); last == nil || last.Reason != release.ReasonStarted {
		t.Errorf("history should record a start: %+v", last)
	}
}

// The plan was read from the host before the operator said yes. If another
// deploy moved the host since, a start of the old release must be refused.
func TestStartRefusesWhenTheLiveReleaseMoved(t *testing.T) {
	var activated []string
	e, p, _ := startExecutor(t, runtime.StateRunning, &activated)
	p.Hosts[0].From, p.Hosts[0].To = "0001-aaaaaaa", "0001-aaaaaaa"

	if _, err := e.Run(t.Context(), p); err == nil {
		t.Fatal("a start of a release the host no longer runs must fail")
	}
	if len(activated) != 0 {
		t.Errorf("activated %v, want nothing", activated)
	}
}

// fakeAgent answers `pilotd ctl` the way an agent would, and keeps the deploy
// request it was sent.
type fakeAgent struct {
	transport.Executor
	sent proto.DeployRequest
}

func (f *fakeAgent) RunInput(_ context.Context, _ string, stdin []byte) (transport.Result, error) {
	if err := json.Unmarshal(stdin, &f.sent); err != nil {
		return transport.Result{}, err
	}
	return transport.Result{Stdout: `{"id":"j1","state":"running"}`}, nil
}

func (f *fakeAgent) Run(context.Context, string) (transport.Result, error) {
	return transport.Result{Stdout: `{"id":"j1","state":"succeeded"}`}, nil
}

// The agent refuses to infer a start, so the CLI must ask for one by name, and
// for the release the host runs rather than the sequence the plan would use.
func TestStartIsHandedToTheAgentAsAStart(t *testing.T) {
	e, p, target := startExecutor(t, runtime.StateRunning, nil)
	agent := &fakeAgent{}
	e.Agents = map[string]*remote.Client{"web-1": remote.New(agent, "web-1", target.Layout)}

	outcomes, err := e.Run(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !agent.sent.Start || agent.sent.Release != "0002-bbbbbbb" {
		t.Errorf("sent start=%v release=%q, want a start of 0002-bbbbbbb", agent.sent.Start, agent.sent.Release)
	}
	if out := outcomes[0]; !out.Succeeded || out.Message != "started" {
		t.Errorf("outcome = %+v", out)
	}
}
