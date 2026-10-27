package deploy

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/build"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/local"
)

// A plan is printed to a terminal and may end up in CI output, so it must name
// which variables changed without ever revealing a value.
func TestDescribeEnvChangeNeverLeaksValues(t *testing.T) {
	tests := []struct {
		name          string
		before, after []string
		want          string
	}{
		{"added", []string{"A"}, []string{"A", "FEATURE_QUEUE"}, "+FEATURE_QUEUE"},
		{"removed", []string{"A", "OLD"}, []string{"A"}, "-OLD"},
		{"both", []string{"A", "OLD"}, []string{"A", "NEW"}, "+NEW, -OLD"},
		{"value changed only", []string{"A"}, []string{"A"}, "values changed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := describeEnvChange(tc.before, tc.after)
			if got != tc.want {
				t.Errorf("describeEnvChange = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	prev := &release.Manifest{
		Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaaaaaaaaaabbbb"}},
		EnvKeys:   []string{"LOG_LEVEL"},
		EnvHash:   "env-1",
		RouteHash: "route-1",
	}

	t.Run("image change is reported by short digest", func(t *testing.T) {
		p := &Plan{
			Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:ccccccccccccdddd"}},
			EnvHash:   "env-1", RouteHash: "route-1",
		}
		got := diff(prev, p)
		if len(got) != 1 || got[0].Field != "image" {
			t.Fatalf("diff = %+v", got)
		}
		if strings.Contains(got[0].From, "sha256:") {
			t.Errorf("digests should be shortened for display, got %q", got[0].From)
		}
		if got[0].To != "cccccccccccc" {
			t.Errorf("To = %q", got[0].To)
		}
	})

	t.Run("identical release yields no changes", func(t *testing.T) {
		p := &Plan{
			Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaaaaaaaaaabbbb"}},
			EnvHash:   "env-1", RouteHash: "route-1",
		}
		if got := diff(prev, p); len(got) != 0 {
			t.Errorf("diff = %+v, want none", got)
		}
	})

	t.Run("route change is detected", func(t *testing.T) {
		p := &Plan{
			Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaaaaaaaaaabbbb"}},
			EnvHash:   "env-1", RouteHash: "route-2",
		}
		got := diff(prev, p)
		if len(got) != 1 || got[0].Field != "route" {
			t.Errorf("diff = %+v", got)
		}
	})
}

func TestPlanNoOp(t *testing.T) {
	tests := []struct {
		name  string
		hosts []HostPlan
		want  bool
	}{
		{"all unchanged", []HostPlan{{NoOp: true}, {NoOp: true}}, true},
		{"one changed", []HostPlan{{NoOp: true}, {NoOp: false}}, false},
		{"none", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plan{Hosts: tc.hosts}
			if got := p.NoOp(); got != tc.want {
				t.Errorf("NoOp = %v, want %v", got, tc.want)
			}
		})
	}
}

// The manifest travels to the host and is read back by `pilot releases`, so it
// records env names and a digest but never a value.
func TestPlanManifestOmitsSecretValues(t *testing.T) {
	p := &Plan{
		Service: &config.Service{
			Name: "api", Runtime: config.RuntimeCompose,
			Source: &config.Source{Repo: "git@github.com:me/api.git", Ref: "main"},
		},
		Release: "0042-9f3ac1b",
		Hash:    "9f3ac1bdeadbeef",
		Commit:  "abc123",
		EnvKeys: []string{"DATABASE_URL", "LOG_LEVEL"},
		EnvHash: release.HashMap(map[string]string{"DATABASE_URL": "postgres://u:secret@db/app"}),
	}

	m := p.Manifest("web-1")
	if err := m.Validate(); err != nil {
		t.Fatalf("generated manifest is invalid: %v", err)
	}
	if m.Sequence != 42 {
		t.Errorf("sequence = %d, want 42 from the release id", m.Sequence)
	}
	if m.Source == nil || m.Source.Commit != "abc123" {
		t.Errorf("source not recorded: %+v", m.Source)
	}

	body, err := release.MarshalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "secret") || strings.Contains(string(body), "postgres://") {
		t.Errorf("manifest leaked an env value:\n%s", body)
	}
	if !strings.Contains(string(body), "DATABASE_URL") {
		t.Error("manifest should record env names")
	}
}

func TestRenderRoute(t *testing.T) {
	layout := release.NewLayout("")
	fleet := &config.Fleet{}

	t.Run("unexposed service has no route", func(t *testing.T) {
		got, _, err := renderRoute(&config.Service{Name: "worker"}, fleet, layout)
		if err != nil || got != "" {
			t.Errorf("got %q, %v", got, err)
		}
	})

	t.Run("static route is rooted at the current symlink", func(t *testing.T) {
		s := &config.Service{
			Name:    "blog",
			Runtime: config.RuntimeStatic,
			Expose: &config.Expose{
				Domains: []string{"blog.example.com"},
				Static:  &config.StaticExpose{Index: "index.html"},
			},
		}
		got, _, err := renderRoute(s, fleet, layout)
		if err != nil {
			t.Fatal(err)
		}
		// Rooting at `current` rather than a release path is what makes a
		// swap take effect without touching Caddy at all.
		if !strings.Contains(got, "root * /opt/pilot/services/blog/current") {
			t.Errorf("route should point at the symlink:\n%s", got)
		}
		if strings.Contains(got, "releases/") {
			t.Errorf("route must not pin a release directory:\n%s", got)
		}
	})

	t.Run("a blue-green route is rendered per colour, shared when its domain is", func(t *testing.T) {
		site := &config.Service{Name: "site", Hosts: []string{"web-1"},
			Expose:  &config.Expose{Domains: []string{"example.com"}, Upstream: 8080},
			Rollout: &config.Rollout{Strategy: config.StrategyBlueGreen, Service: "web", Ports: []int{18080, 18081}}}
		api := &config.Service{Name: "api", Hosts: []string{"web-1"}, Expose: &config.Expose{Domains: []string{"example.com"}, Path: "/v1", Upstream: 8081}}
		fleet := &config.Fleet{Services: map[string]*config.Service{"site": site, "api": api}}

		blue, colors, err := renderRoute(site, fleet, layout)
		if err != nil {
			t.Fatal(err)
		}
		if blue != colors["blue"] || len(colors) != 2 {
			t.Fatalf("the release's single route must be blue's, got %d colours", len(colors))
		}
		for color, port := range map[string]string{"blue": "18080", "green": "18081"} {
			got := colors[color]
			if !strings.Contains(got, "\n# shared site: example.com\n") || !strings.Contains(got, "reverse_proxy 127.0.0.1:"+port+" ") {
				t.Errorf("%s route should be a shared handle at %s:\n%s", color, port, got)
			}
		}
	})

	// The whole-domain service must leave the site block to Pilot as soon as
	// a path service shares its domain, or Caddy would see two blocks for it.
	t.Run("a whole-domain route beside a path route is shared", func(t *testing.T) {
		site := &config.Service{Name: "site", Hosts: []string{"web-1"}, Expose: &config.Expose{Domains: []string{"example.com"}, Upstream: 8080}}
		api := &config.Service{Name: "api", Hosts: []string{"web-1"}, Expose: &config.Expose{Domains: []string{"example.com"}, Path: "/v1", Upstream: 8081}}
		fleet := &config.Fleet{Services: map[string]*config.Service{"site": site, "api": api}}

		for _, s := range []*config.Service{site, api} {
			got, _, err := renderRoute(s, fleet, layout)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, "\n# shared site: example.com\n") || strings.Contains(got, "example.com {") {
				t.Errorf("%s should render as handles in the shared site:\n%s", s.Name, got)
			}
		}
	})
}

// Two deploys with identical inputs must produce the same hash, so that
// "nothing actually changed" is visible rather than silently re-shipped.
func TestReleaseHashIsStableForIdenticalInputs(t *testing.T) {
	hashFor := func(env map[string]string, digest string) string {
		h := release.NewHasher()
		h.AddString("runtime", "compose")
		h.AddString("env", release.HashMap(env))
		h.AddString("route", "route-1")
		h.AddString("artifact:image", digest)
		return h.Sum()
	}

	base := hashFor(map[string]string{"LOG_LEVEL": "info"}, "sha256:aaa")
	same := hashFor(map[string]string{"LOG_LEVEL": "info"}, "sha256:aaa")
	if base != same {
		t.Error("identical inputs produced different release hashes")
	}

	if base == hashFor(map[string]string{"LOG_LEVEL": "debug"}, "sha256:aaa") {
		t.Error("an env change must change the release hash")
	}
	if base == hashFor(map[string]string{"LOG_LEVEL": "info"}, "sha256:bbb") {
		t.Error("an image change must change the release hash")
	}
}

func TestShortDigest(t *testing.T) {
	tests := []struct{ in, want string }{
		{"sha256:aaaaaaaaaaaabbbbcccc", "aaaaaaaaaaaa"},
		{"short", "short"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := shortDigest(tc.in); got != tc.want {
			t.Errorf("shortDigest(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A service with no route has no route to leave unchanged. Requiring one made
// every systemd unit and worker redeploy identical content under a new sequence
// number, restarting it each time.
func TestPlanUnexposedServiceCanBeANoOp(t *testing.T) {
	daemon := func() *config.Service {
		return &config.Service{Name: "worker", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"}}
	}
	oneshot := func() *config.Service {
		return &config.Service{
			Name: "backup", Runtime: config.RuntimeSystemd, Hosts: []string{"web-1"},
			Unit: &config.Unit{Name: "backup.service", Kind: config.UnitOneshot, Timer: "backup.timer"},
		}
	}
	runner := func() *config.Service {
		return &config.Service{
			Name: "gh-runner", Runtime: config.RuntimeSystemd, Hosts: []string{"web-1"},
			Unit: &config.Unit{Name: "gh-runner.service", Kind: config.UnitService},
		}
	}
	exited := []runtime.Instance{{Name: "worker-1", State: "exited"}}
	running := runtime.Observation{State: runtime.StateRunning}

	tests := []struct {
		name      string
		svc       func() *config.Service
		obs       runtime.Observation
		digest    string
		edit      func(*config.Service)
		wantNoOp  bool
		wantStart bool
	}{
		{"unchanged and running", daemon, running, "", nil, true, false},
		{"unchanged and stopped", daemon, runtime.Observation{State: runtime.StateStopped, Instances: exited}, "", nil, false, true},
		{"oneshot between runs", oneshot, runtime.Observation{State: runtime.StateStopped, Instances: exited}, "", nil, true, false},
		{"new image", daemon, running, "sha256:bbbb", nil, false, false},
		{"unit unchanged", runner, running, "", nil, true, false},
		// Before a service with no route could be a no-op, this applied only
		// because every deploy restarted the unit.
		{"unit links changed", runner, running, "", func(s *config.Service) {
			s.Unit.Links = map[string]string{"/usr/local/bin/runner": "bin/runner"}
		}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in, live := liveInput(t, tc.svc())
			in.Runtime = observer{stubRuntime: stubRuntime{tc.obs}}
			if tc.digest != "" {
				in.Build = &build.Result{Artifacts: []release.Artifact{{Name: "image", Digest: tc.digest}}}
			}
			if tc.edit != nil {
				tc.edit(in.Service)
			}

			p, err := Compute(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			hp := p.Hosts[0]
			if hp.NoOp != tc.wantNoOp || hp.Start != tc.wantStart {
				t.Fatalf("noop=%v start=%v, want noop=%v start=%v", hp.NoOp, hp.Start, tc.wantNoOp, tc.wantStart)
			}
			if hp.Route != RouteNotExposed {
				t.Errorf("route = %q, want %q", hp.Route, RouteNotExposed)
			}
			if (tc.wantNoOp || tc.wantStart) && hp.To != live {
				t.Errorf("to = %q, want the live release %q", hp.To, live)
			}
		})
	}
}

// Removing a build output, or anything else the hash covers that diff does not
// name, must still read as a change rather than as nothing to do.
func TestDiffCatchesWhatTheFieldsMiss(t *testing.T) {
	prev := &release.Manifest{
		Release: "0007-aaaaaaa", Hash: "aaaa",
		Artifacts: []release.Artifact{
			{Name: "image", Digest: "sha256:aaaaaaaaaaaabbbb"},
			{Name: "dist/", Digest: "sha256:ccccccccccccdddd"},
		},
		Activation: map[string]string{},
	}

	t.Run("removed artifact", func(t *testing.T) {
		p := &Plan{Release: "0008-bbbbbbb", Hash: "bbbb", Artifacts: prev.Artifacts[:1]}
		got := diff(prev, p)
		if len(got) != 1 || got[0].Field != "dist/" || got[0].To != "removed" {
			t.Errorf("diff = %+v", got)
		}
	})

	t.Run("hash differs and no field does", func(t *testing.T) {
		p := &Plan{Release: "0008-bbbbbbb", Hash: "bbbb", Artifacts: prev.Artifacts}
		got := diff(prev, p)
		if len(got) != 1 || got[0].Field != "release" || got[0].From != "0007-aaaaaaa" {
			t.Errorf("diff = %+v", got)
		}
	})
}

// planFor is the part of a plan that diffActivation reads.
func planFor(s *config.Service) *Plan { return &Plan{Service: s, Activation: activation(s)} }

// Configuration read only at stage or activation sits outside the release
// hash. Unexposed services used to apply it by redeploying every time; once
// they can be a no-op, a change to it has to show as a change.
func TestDiffActivation(t *testing.T) {
	base := func() *config.Service {
		return &config.Service{
			Name: "gh-runner", Runtime: config.RuntimeSystemd,
			Unit: &config.Unit{
				Name: "gh-runner.service", Kind: config.UnitService,
				Links:    map[string]string{"/usr/local/bin/runner": "bin/runner"},
				Precheck: []string{"./bin/runner", "--check"},
			},
			Compose: &config.Compose{Project: "runner"},
		}
	}

	tests := []struct {
		field string
		edit  func(*config.Service)
	}{
		{"unit.name", func(s *config.Service) { s.Unit.Name = "gh-runner-2.service" }},
		{"unit.kind", func(s *config.Service) { s.Unit.Kind = config.UnitOneshot }},
		{"unit.timer", func(s *config.Service) { s.Unit.Timer = "gh-runner.timer" }},
		{"unit.links", func(s *config.Service) { s.Unit.Links["/usr/local/bin/runner"] = "bin/runner-v2" }},
		{"unit.precheck", func(s *config.Service) { s.Unit.Precheck = []string{"./bin/runner", "--validate"} }},
		{"compose.project", func(s *config.Service) { s.Compose.Project = "runner-2" }},
		{"rollout.strategy", func(s *config.Service) {
			s.Rollout = &config.Rollout{Strategy: config.StrategyBlueGreen, Service: "web", Ports: []int{18080, 18081}}
		}},
	}
	prev := &release.Manifest{Activation: activation(base())}
	for _, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			s := base()
			tc.edit(s)
			got := diff(prev, planFor(s))
			if !slices.ContainsFunc(got, func(c Change) bool { return c.Field == tc.field }) {
				t.Errorf("diff = %+v, want a change to %s", got, tc.field)
			}
		})
	}

	t.Run("unchanged", func(t *testing.T) {
		if got := diff(prev, planFor(base())); len(got) != 0 {
			t.Errorf("diff = %+v, want none", got)
		}
	})

	// Pacing and health are read from the definition each time, so they never
	// need a deploy to take effect.
	t.Run("pacing and health", func(t *testing.T) {
		s := base()
		s.Rollout = &config.Rollout{Strategy: config.StrategyRecreate, PauseBetween: config.Duration(time.Minute)}
		s.Health = &config.Health{Systemd: true}
		if got := diff(prev, planFor(s)); len(got) != 0 {
			t.Errorf("diff = %+v, want none", got)
		}
	})

	t.Run("release from before the record", func(t *testing.T) {
		old := &release.Manifest{}
		got := diff(old, planFor(base()))
		if len(got) != 1 || got[0].Field != "activation" {
			t.Errorf("diff = %+v, want one change saying it was not recorded", got)
		}
		if got := diff(old, planFor(&config.Service{Name: "web"})); len(got) != 0 {
			t.Errorf("a service with nothing to record should not redeploy: %+v", got)
		}
	})
}

// The record has to survive the trip to the host and back, or every deploy
// would read the live release as predating it.
func TestPlanRecordsActivation(t *testing.T) {
	s := &config.Service{
		Name: "kite", Runtime: config.RuntimeSystemd, Hosts: []string{"web-1"},
		Unit:   &config.Unit{Name: "kite.service", Kind: config.UnitService},
		Expose: &config.Expose{Domains: []string{"kite.example.com"}, Upstream: 8080},
	}
	in, _ := liveInput(t, s)

	p, err := Compute(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Hosts[0].NoOp {
		t.Fatalf("an unchanged unit should be a no-op: %+v", p.Hosts[0])
	}

	s.Unit.Links = map[string]string{"/usr/local/bin/kite": "kite"}
	p, err = Compute(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if hp := p.Hosts[0]; hp.NoOp || len(hp.Changes) != 1 || hp.Changes[0].Field != "unit.links" {
		t.Errorf("noop=%v changes=%+v, want a unit.links change", hp.NoOp, hp.Changes)
	}
}

// Compose scopes containers by project, so a new project name or strategy
// starts new containers and leaves the old ones running. The plan has to say
// so, with the command that cleans up.
func TestLeftBehind(t *testing.T) {
	recreate := func() *config.Service {
		return &config.Service{Name: "kite", Runtime: config.RuntimeCompose, Compose: &config.Compose{Project: "kite-2"}}
	}
	blueGreen := func() *config.Service {
		s := recreate()
		s.Rollout = &config.Rollout{Strategy: config.StrategyBlueGreen}
		return s
	}

	tests := []struct {
		name    string
		svc     func() *config.Service
		changes []Change
		want    []string
	}{
		{"project renamed", recreate,
			[]Change{{Field: "compose.project", To: "kite-2"}},
			[]string{"kite → kite-2 leaves kite running", "docker compose -p kite down"}},
		{"to blue-green", blueGreen,
			[]Change{{Field: "rollout.strategy", To: config.StrategyBlueGreen}},
			[]string{"leaves the recreate stack kite-2 running", "docker compose -p kite-2 down"}},
		{"to blue-green and renamed", blueGreen,
			[]Change{{Field: "compose.project", To: "kite-2"}, {Field: "rollout.strategy", To: config.StrategyBlueGreen}},
			[]string{"recreate stack kite running", "docker compose -p kite down"}},
		{"back to recreate", recreate,
			[]Change{{Field: "rollout.strategy", From: config.StrategyBlueGreen}},
			[]string{"kite-blue and kite-green", "docker compose -p kite-blue down", "docker compose -p kite-green down"}},
		{"project renamed under blue-green", blueGreen,
			[]Change{{Field: "compose.project", To: "kite-2"}}, nil},
		{"links changed", recreate,
			[]Change{{Field: "unit.links", To: "a -> b"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := leftBehind(tc.svc(), tc.changes)
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Errorf("warnings = %q, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("warnings = %q, want one", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got[0], want) {
					t.Errorf("warning %q lacks %q", got[0], want)
				}
			}
		})
	}
}

func TestPlanCarriesWarnings(t *testing.T) {
	s := &config.Service{
		Name: "kite", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Compose: &config.Compose{Project: "kite"},
		Expose:  &config.Expose{Domains: []string{"kite.example.com"}, Upstream: 8080},
	}
	in, _ := liveInput(t, s)
	s.Compose.Project = "kite-2"

	p, err := Compute(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if w := p.Hosts[0].Warnings; len(w) != 1 || !strings.Contains(w[0], "docker compose -p kite down") {
		t.Errorf("warnings = %q", w)
	}
}

// Omitting `kind` means a service, so writing it out is not a change.
func TestActivationAppliesTheUnitKindDefault(t *testing.T) {
	omitted := &config.Service{Name: "runner", Unit: &config.Unit{Name: "runner.service"}}
	written := &config.Service{Name: "runner", Unit: &config.Unit{Name: "runner.service", Kind: config.UnitService}}
	if got := diff(&release.Manifest{Activation: activation(omitted)}, planFor(written)); len(got) != 0 {
		t.Errorf("diff = %+v, want none", got)
	}
}

// A path route arriving on a domain whose whole-domain service still has a
// site block of its own is refused at plan time: at install it would come
// after activation, and Caddy would reject the shared site block anyway.
func TestPlanRefusesASharedSiteBesideAnOldBlock(t *testing.T) {
	root := t.TempDir()
	layout := release.NewLayout(filepath.Join(root, "pilot"))
	site := &config.Service{Name: "site", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Expose: &config.Expose{Domains: []string{"example.com"}, Upstream: 8080}}
	api := &config.Service{Name: "api", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Expose: &config.Expose{Domains: []string{"example.com"}, Path: "/v1", Upstream: 8081}}
	fleet := &config.Fleet{
		Caddy:    config.Caddy{SnippetDir: filepath.Join(root, "pilot.d")},
		Services: map[string]*config.Service{"site": site, "api": api},
	}
	write(t, filepath.Join(fleet.Caddy.SnippetDir, "site.caddy"), "example.com {\n\treverse_proxy 127.0.0.1:8080\n}\n")

	in := Input{
		Service: api, Fleet: fleet, Layout: layout,
		Build: &build.Result{Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaa"}}},
		Targets: map[string]*runtime.Target{"web-1": {
			Service: api, Host: &config.Host{Name: "web-1"}, Layout: layout, Client: local.New("web-1"),
		}},
	}
	_, err := Compute(t.Context(), in)
	if err == nil || !strings.Contains(err.Error(), "first deploy site") {
		t.Fatalf("want the plan refused, naming site, got: %v", err)
	}

	in.Service, in.Targets["web-1"].Service = site, site
	if _, err := Compute(t.Context(), in); err != nil {
		t.Errorf("site itself moves its own block into the shared site: %v", err)
	}
}

// A held route lock is worth knowing before a deploy: the route change waits
// for it, and an update that gives up after activation rolls the code back.
func TestPlanWarnsAboutAHeldRouteLock(t *testing.T) {
	root := t.TempDir()
	layout := release.NewLayout(filepath.Join(root, "pilot"))
	api := &config.Service{Name: "api", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Expose: &config.Expose{Domains: []string{"api.example.com"}, Upstream: 8081}}
	fleet := &config.Fleet{
		Caddy:    config.Caddy{SnippetDir: filepath.Join(root, "pilot.d")},
		Services: map[string]*config.Service{"api": api},
	}
	in := Input{
		Service: api, Fleet: fleet, Layout: layout,
		Build: &build.Result{Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaa"}}},
		Targets: map[string]*runtime.Target{"web-1": {
			Service: api, Host: &config.Host{Name: "web-1"}, Layout: layout, Client: local.New("web-1"),
		}},
	}
	warned := func() bool {
		p, err := Compute(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(p.Hosts[0].Warnings, func(w string) bool { return strings.Contains(w, ".pilot-lock") })
	}

	if warned() {
		t.Error("warned about a lock nobody holds")
	}
	write(t, filepath.Join(fleet.Caddy.SnippetDir, ".pilot-lock", "owner"), "abc\n")
	if !warned() {
		t.Error("no warning while another Pilot holds the route lock")
	}
}

// Under blue-green, expose.upstream is the container port and only the colours'
// host ports are reachable, so no route may point at the container port. The
// plan compares the installed route with the serving colour's, so a host that
// already serves this release is a no-op whichever colour serves it.
func TestPlanRoutesBlueGreenToTheColourPorts(t *testing.T) {
	root := t.TempDir()
	layout := release.NewLayout(filepath.Join(root, "pilot"))
	api := &config.Service{Name: "api", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Expose:  &config.Expose{Domains: []string{"api.example.com"}, Upstream: 8080},
		Rollout: &config.Rollout{Strategy: config.StrategyBlueGreen, Service: "web", Ports: []int{18080, 18081}}}
	fleet := &config.Fleet{
		Caddy:    config.Caddy{SnippetDir: filepath.Join(root, "pilot.d")},
		Services: map[string]*config.Service{"api": api},
	}
	target := &runtime.Target{Service: api, Host: &config.Host{Name: "web-1"}, Layout: layout, Client: local.New("web-1")}
	in := Input{
		Service: api, Fleet: fleet, Layout: layout,
		Build:   &build.Result{Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaa"}}},
		Targets: map[string]*runtime.Target{"web-1": target},
	}

	p, err := Compute(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.Snippet, "127.0.0.1:8080 ") || !strings.Contains(p.Snippet, "127.0.0.1:18080 ") {
		t.Fatalf("the plan's route must point at a colour's host port, not the container port:\n%s", p.Snippet)
	}

	green := strings.Replace(p.Snippet, "127.0.0.1:18080 ", "127.0.0.1:18081 ", 1)
	write(t, filepath.Join(fleet.Caddy.SnippetDir, "api.caddy"), green)
	st := release.NewState("api")
	st.ActiveColor = "green"
	if err := runtime.WriteState(t.Context(), target, st); err != nil {
		t.Fatal(err)
	}
	if p, err = Compute(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if got := p.Hosts[0].Route; got != RouteNone {
		t.Errorf("green serves and the installed route is green's, yet the plan says %q", got)
	}

	st.ActiveColor = "blue"
	if err := runtime.WriteState(t.Context(), target, st); err != nil {
		t.Fatal(err)
	}
	if p, err = Compute(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if got := p.Hosts[0].Route; got != RouteUpdate {
		t.Errorf("blue serves but green's route is installed, yet the plan says %q", got)
	}

	// Guessing blue for an unreadable state would read green's live route as
	// changed, and flip onto the stack that serves.
	write(t, layout.State("api"), "{not json")
	if _, err := Compute(t.Context(), in); err == nil || !strings.Contains(err.Error(), "state") {
		t.Errorf("an unreadable state was guessed: %v", err)
	}
}

// A route hash that changed for services that are not blue-green would make
// every one of them ship a new release on the next deploy for no reason.
func TestRouteHashIsUnchangedOutsideBlueGreen(t *testing.T) {
	root := t.TempDir()
	layout := release.NewLayout(filepath.Join(root, "pilot"))
	api := &config.Service{Name: "api", Runtime: config.RuntimeCompose, Hosts: []string{"web-1"},
		Expose: &config.Expose{Domains: []string{"api.example.com"}, Upstream: 8080}}
	in := Input{
		Service: api, Fleet: &config.Fleet{Caddy: config.Caddy{SnippetDir: filepath.Join(root, "pilot.d")}}, Layout: layout,
		Build: &build.Result{Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaa"}}},
		Targets: map[string]*runtime.Target{"web-1": {
			Service: api, Host: &config.Host{Name: "web-1"}, Layout: layout, Client: local.New("web-1"),
		}},
	}
	p, err := Compute(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if p.RouteHash != release.HashBytes([]byte(p.Snippet)) {
		t.Error("the route hash of a service that is not blue-green must be the hash of its route alone")
	}
}

// Loading fills in the service name as the compose project. Recording that
// default made every compose service ship once after the upgrade, not only the
// ones with a project of their own.
func TestDiffActivationComposeProjectDefault(t *testing.T) {
	service := func(project string) *config.Service {
		return &config.Service{Name: "kite", Runtime: config.RuntimeCompose, Compose: &config.Compose{Project: project}}
	}
	plan := func(project string) *Plan { return planFor(service(project)) }
	recorded := func(project string) *release.Manifest {
		return &release.Manifest{Activation: activation(service(project))}
	}

	tests := []struct {
		name  string
		prev  *release.Manifest
		plan  *Plan
		field string
	}{
		{"default project, release from before the record", &release.Manifest{}, plan("kite"), ""},
		{"custom project, release from before the record", &release.Manifest{}, plan("kite-2"), "activation"},
		{"default to custom", recorded("kite"), plan("kite-2"), "compose.project"},
		{"custom to default", recorded("kite-2"), plan("kite"), "compose.project"},
		{"default unchanged", recorded("kite"), plan("kite"), ""},
		// v0.16.0 recorded the default explicitly.
		{"default recorded by v0.16.0", &release.Manifest{Activation: map[string]string{"compose.project": "kite"}}, plan("kite"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := diff(tc.prev, tc.plan)
			if tc.field == "" {
				if len(got) != 0 {
					t.Errorf("diff = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Field != tc.field {
				t.Fatalf("diff = %+v, want one change to %s", got, tc.field)
			}
			if tc.field == "compose.project" && (got[0].From == "" || got[0].To == "") {
				t.Errorf("a default project should be named, not blank: %+v", got[0])
			}
		})
	}
}
