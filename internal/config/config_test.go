package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write lays out a fleet on disk. Keys of svcs are service file basenames.
func write(t *testing.T, fleet string, svcs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FleetFile), []byte(fleet), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(svcs) > 0 {
		dir := filepath.Join(root, ServicesDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range svcs {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

const baseFleet = `
version: 1
hosts:
  web-1:
    address: web1.example.com
    tags: [prod, edge]
  box-1:
    address: 10.0.0.5
    tags: [prod]
`

// load is the common path: parse, expect no hard error, return diagnostics.
func load(t *testing.T, fleet string, svcs map[string]string) (*Fleet, Diagnostics) {
	t.Helper()
	f, ds, err := Load(write(t, fleet, svcs))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return f, ds
}

// findDiag returns the first diagnostic whose field and message both match.
func findDiag(ds Diagnostics, field, substr string) *Diagnostic {
	for i := range ds {
		if ds[i].Field == field && strings.Contains(ds[i].Message, substr) {
			return &ds[i]
		}
	}
	return nil
}

func assertNoErrors(t *testing.T, ds Diagnostics) {
	t.Helper()
	if ds.HasErrors() {
		for _, d := range ds.Sorted() {
			if d.Severity == SevError {
				t.Errorf("unexpected error: %s", d)
			}
		}
	}
}

func TestLoadValidFleet(t *testing.T) {
	f, ds := load(t, baseFleet, map[string]string{
		"api.yaml": `
name: api
runtime: compose
hosts: [web-1]
compose: {file: deploy/compose.yaml}
expose:
  domains: [api.example.com]
  upstream: 8080
health: {http: {url: "http://localhost:8080/healthz"}, timeout: 90s}
`,
		"blog.yaml": `
name: blog
runtime: static
hosts: [web-1]
build: {command: "npm run build", output: [dist/]}
expose:
  domains: [blog.example.com]
  static: {spa: true}
health: {http: {url: "https://blog.example.com"}}
`,
	})
	assertNoErrors(t, ds)

	if got := len(f.Services); got != 2 {
		t.Fatalf("services = %d, want 2", got)
	}
	if got := f.ServiceNames(); got[0] != "api" || got[1] != "blog" {
		t.Errorf("ServiceNames = %v, want sorted [api blog]", got)
	}
	if f.Hosts["web-1"].Name != "web-1" {
		t.Error("host Name not backfilled from map key")
	}
}

func TestDefaultsApplied(t *testing.T) {
	f, ds := load(t, `
version: 1
defaults:
  user: deploy
  keep_releases: 3
  health: {timeout: 45s}
hosts:
  web-1: {address: web1.example.com}
`, map[string]string{
		"api.yaml": `
name: api
runtime: compose
hosts: [web-1]
compose: {file: compose.yaml}
health: {http: {url: "http://localhost:8080/up"}}
`,
	})
	assertNoErrors(t, ds)

	if got := f.Hosts["web-1"].User; got != "deploy" {
		t.Errorf("host user = %q, want inherited %q", got, "deploy")
	}
	s := f.Services["api"]
	if s.Manage != ManageDeploy {
		t.Errorf("manage = %q, want %q", s.Manage, ManageDeploy)
	}
	if s.KeepReleases != 3 {
		t.Errorf("keep_releases = %d, want inherited 3", s.KeepReleases)
	}
	if s.Compose.Project != "api" {
		t.Errorf("compose project = %q, want service name", s.Compose.Project)
	}
	if s.Health.Timeout.Duration() != 45*time.Second {
		t.Errorf("health timeout = %s, want inherited 45s", s.Health.Timeout)
	}
	if s.Health.HTTP.Expect != 200 {
		t.Errorf("http expect = %d, want defaulted 200", s.Health.HTTP.Expect)
	}
	if s.Rollout.Strategy != StrategyRecreate || s.Rollout.Concurrency != 1 {
		t.Errorf("rollout = %+v, want recreate/1", *s.Rollout)
	}
	if f.Caddy.SnippetDir != DefaultSnippetDir || f.Caddy.Admin != DefaultCaddyAdmin {
		t.Errorf("caddy defaults not applied: %+v", f.Caddy)
	}
}

// A typo in a config file should fail loudly rather than being ignored.
func TestUnknownFieldRejected(t *testing.T) {
	_, ds := load(t, baseFleet, map[string]string{
		"api.yaml": `
name: api
runtime: compose
hosts: [web-1]
compose: {file: compose.yaml}
expose:
  domains: [api.example.com]
  upstrem: 8080
`,
	})
	if !ds.HasErrors() {
		t.Fatal("expected an error for the misspelled `upstrem` field")
	}
	if !strings.Contains(ds[0].Message, "upstrem") {
		t.Errorf("error should name the offending field, got: %s", ds[0].Message)
	}
}

// One unparseable service file must not hide problems in the others.
func TestParseFailureIsolatedPerFile(t *testing.T) {
	f, ds := load(t, baseFleet, map[string]string{
		"broken.yaml": "name: broken\n  runtime: [[[\n",
		"api.yaml": `
name: api
runtime: nonsense
hosts: [web-1]
`,
	})
	if _, ok := f.Services["api"]; !ok {
		t.Fatal("api should still load when broken.yaml fails to parse")
	}
	if findDiag(ds, "runtime", "nonsense") == nil {
		t.Error("validation of api.yaml should still have run")
	}
}

func TestServiceNameFilenameMismatchWarns(t *testing.T) {
	_, ds := load(t, baseFleet, map[string]string{
		"web.yaml": `
name: api
runtime: compose
hosts: [web-1]
compose: {file: compose.yaml}
health: {docker: true}
`,
	})
	assertNoErrors(t, ds)
	if findDiag(ds, "name", "does not match filename") == nil {
		t.Error("expected a warning about the name/filename mismatch")
	}
}

func TestValidateRuntimeAndHostRefs(t *testing.T) {
	tests := []struct {
		name  string
		svc   string
		field string
		want  string
	}{
		{"unknown runtime", "name: x\nruntime: podman\nhosts: [web-1]\n", "runtime", "unknown runtime"},
		{"missing runtime", "name: x\nhosts: [web-1]\n", "runtime", "missing"},
		{"unknown host", "name: x\nruntime: compose\nhosts: [nope]\ncompose: {file: c.yaml}\n", "hosts[0]", "no such host"},
		{"no hosts", "name: x\nruntime: compose\ncompose: {file: c.yaml}\n", "hosts", "missing"},
		{"compose without file", "name: x\nruntime: compose\nhosts: [web-1]\n", "compose.file", "missing"},
		{"systemd without unit name", "name: x\nruntime: systemd\nhosts: [web-1]\n", "unit.name", "missing"},
		{"static without build", "name: x\nruntime: static\nhosts: [web-1]\n", "build.output", "missing"},
		{"unit on compose", "name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\nunit: {name: x.service}\n", "unit", "only valid for runtime"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := load(t, baseFleet, map[string]string{"x.yaml": tc.svc})
			if findDiag(ds, tc.field, tc.want) == nil {
				t.Errorf("want %s: %q; got:\n%v", tc.field, tc.want, ds.Sorted())
			}
		})
	}
}

func TestValidateExpose(t *testing.T) {
	tests := []struct {
		name  string
		svc   string
		field string
		want  string
	}{
		{
			"both upstream and static",
			"name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\nexpose: {domains: [a.com], upstream: 8080, static: true}\n",
			"expose", "both `upstream` and `static`",
		},
		{
			"neither",
			"name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\nexpose: {domains: [a.com]}\n",
			"expose", "neither",
		},
		{
			"no domains",
			"name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\nexpose: {upstream: 8080}\n",
			"expose.domains", "missing",
		},
		{
			"domain with scheme",
			"name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\nexpose: {domains: [\"https://a.com\"], upstream: 8080}\n",
			"expose.domains[0]", "includes a scheme",
		},
		{
			"static service cannot proxy",
			"name: x\nruntime: static\nhosts: [web-1]\nbuild: {command: b, output: [d/]}\nexpose: {domains: [a.com], upstream: 8080}\n",
			"expose.upstream", "no process to proxy to",
		},
		{
			"compose service cannot file-serve",
			"name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\nexpose: {domains: [a.com], static: true}\n",
			"expose.static", "serves from a process",
		},
		{
			"path must be absolute",
			"name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\nexpose: {domains: [a.com], path: \"v1/*\", upstream: 8080}\n",
			"expose.path", "must start with /",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := load(t, baseFleet, map[string]string{"x.yaml": tc.svc})
			if findDiag(ds, tc.field, tc.want) == nil {
				t.Errorf("want %s: %q; got:\n%v", tc.field, tc.want, ds.Sorted())
			}
		})
	}
}

// `static: true` and `static: {spa: true}` must both work — the bare-bool form
// reads better for a plain file server.
func TestStaticExposeAcceptsBoolAndMap(t *testing.T) {
	for _, form := range []string{"true", "{spa: true}", "{}"} {
		t.Run(form, func(t *testing.T) {
			f, ds := load(t, baseFleet, map[string]string{
				"blog.yaml": "name: blog\nruntime: static\nhosts: [web-1]\n" +
					"build: {command: b, output: [dist/]}\n" +
					"expose: {domains: [blog.example.com], static: " + form + "}\n" +
					"health: {http: {url: \"https://blog.example.com\"}}\n",
			})
			assertNoErrors(t, ds)
			e := f.Services["blog"].Expose
			if !e.IsStatic() {
				t.Fatalf("static form %q did not produce a static route", form)
			}
			if e.Static.Index != DefaultIndex {
				t.Errorf("index = %q, want defaulted %q", e.Static.Index, DefaultIndex)
			}
			if form == "{spa: true}" && !e.Static.SPA {
				t.Error("spa flag lost")
			}
		})
	}
}

// Two services claiming one address on one host is a Caddy reload failure
// waiting to happen; the same address on different hosts is normal.
func TestRouteCollisions(t *testing.T) {
	// Distinct ports per service: two stacks on one host cannot both bind the
	// same one, and that is checked separately.
	ports := map[string]string{"a": "8080", "b": "8081"}
	svc := func(name, host, domain, path string) string {
		s := "name: " + name + "\nruntime: compose\nhosts: [" + host + "]\n" +
			"compose: {file: c.yaml}\nhealth: {docker: true}\n" +
			"expose: {domains: [" + domain + "], upstream: " + ports[name]
		if path != "" {
			s += ", path: \"" + path + "\""
		}
		return s + "}\n"
	}

	t.Run("same host and address collides", func(t *testing.T) {
		_, ds := load(t, baseFleet, map[string]string{
			"a.yaml": svc("a", "web-1", "shared.example.com", ""),
			"b.yaml": svc("b", "web-1", "shared.example.com", ""),
		})
		d := findDiag(ds, "expose.domains", "also claimed by")
		if d == nil {
			t.Fatalf("expected a collision; got:\n%v", ds.Sorted())
		}
		// Reported against both participants, so it's visible from either file.
		if n := len(ds); n != 2 {
			t.Errorf("got %d diagnostics, want one per participant", n)
		}
	})

	t.Run("different hosts do not collide", func(t *testing.T) {
		_, ds := load(t, baseFleet, map[string]string{
			"a.yaml": svc("a", "web-1", "shared.example.com", ""),
			"b.yaml": svc("b", "box-1", "shared.example.com", ""),
		})
		assertNoErrors(t, ds)
	})

	t.Run("distinct paths do not collide", func(t *testing.T) {
		_, ds := load(t, baseFleet, map[string]string{
			"a.yaml": svc("a", "web-1", "shared.example.com", "/api/*"),
			"b.yaml": svc("b", "web-1", "shared.example.com", ""),
		})
		assertNoErrors(t, ds)
	})
}

func TestValidateHealth(t *testing.T) {
	t.Run("multiple probes rejected", func(t *testing.T) {
		_, ds := load(t, baseFleet, map[string]string{
			"x.yaml": "name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\n" +
				"health: {docker: true, tcp: {addr: \"localhost:1\"}}\n",
		})
		if findDiag(ds, "health", "2 probes") == nil {
			t.Errorf("want a multi-probe error; got:\n%v", ds.Sorted())
		}
	})

	t.Run("wrong probe for runtime", func(t *testing.T) {
		_, ds := load(t, baseFleet, map[string]string{
			"x.yaml": "name: x\nruntime: static\nhosts: [web-1]\nbuild: {command: b, output: [d/]}\n" +
				"health: {docker: true}\n",
		})
		if findDiag(ds, "health.docker", "only valid for the compose runtime") == nil {
			t.Errorf("want a runtime mismatch error; got:\n%v", ds.Sorted())
		}
	})

	t.Run("missing health warns but does not block", func(t *testing.T) {
		_, ds := load(t, baseFleet, map[string]string{
			"x.yaml": "name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\n",
		})
		assertNoErrors(t, ds)
		if findDiag(ds, "health", "no health check") == nil {
			t.Errorf("want a warning; got:\n%v", ds.Sorted())
		}
	})

	t.Run("interval beyond timeout rejected", func(t *testing.T) {
		_, ds := load(t, baseFleet, map[string]string{
			"x.yaml": "name: x\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\n" +
				"health: {docker: true, timeout: 10s, interval: 30s}\n",
		})
		if findDiag(ds, "health.interval", "exceeds timeout") == nil {
			t.Errorf("want an interval error; got:\n%v", ds.Sorted())
		}
	})
}

// A database must be monitorable without being deployable.
func TestManageObserve(t *testing.T) {
	f, ds := load(t, baseFleet, map[string]string{
		"postgres.yaml": `
name: postgres
runtime: compose
hosts: [box-1]
manage: observe
health: {exec: {cmd: [pg_isready, -U, postgres]}}
`,
	})
	assertNoErrors(t, ds)

	s := f.Services["postgres"]
	if s.Deployable() {
		t.Error("observe-only service reported as deployable")
	}
	// compose.file is required to deploy, but an observed service is never
	// staged, so its absence must not be an error.
	if findDiag(ds, "compose.file", "missing") != nil {
		t.Error("observe-only service should not require compose.file")
	}
}

func TestManageObserveWarnsOnBuild(t *testing.T) {
	_, ds := load(t, baseFleet, map[string]string{
		"pg.yaml": `
name: pg
runtime: compose
hosts: [box-1]
manage: observe
build: {image: "ghcr.io/me/pg"}
health: {docker: true}
`,
	})
	assertNoErrors(t, ds)
	if findDiag(ds, "build", "ignored for `manage: observe`") == nil {
		t.Errorf("want a warning that build is ignored; got:\n%v", ds.Sorted())
	}
}

func TestResolveTargets(t *testing.T) {
	f, ds := load(t, baseFleet, map[string]string{
		"api.yaml": "name: api\nruntime: compose\nhosts: [web-1]\ncompose: {file: c.yaml}\nhealth: {docker: true}\n",
		"job.yaml": "name: job\nruntime: compose\nhosts: [box-1]\ncompose: {file: c.yaml}\nhealth: {docker: true}\n",
	})
	assertNoErrors(t, ds)

	t.Run("by name", func(t *testing.T) {
		got, err := f.ResolveTargets("api")
		if err != nil || len(got) != 1 || got[0].Name != "api" {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("by tag", func(t *testing.T) {
		got, err := f.ResolveTargets("@prod")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("@prod matched %d services, want 2", len(got))
		}
	})
	t.Run("narrow tag", func(t *testing.T) {
		got, err := f.ResolveTargets("@edge")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != "api" {
			t.Errorf("@edge = %v, want [api]", got)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		if _, err := f.ResolveTargets("nope"); err == nil {
			t.Error("want an error for an unknown service")
		}
		if _, err := f.ResolveTargets("@nope"); err == nil {
			t.Error("want an error for an unmatched tag")
		}
	})
}

func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"90s", 90 * time.Second, false},
		{"5m", 5 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"30", 30 * time.Second, false}, // bare number reads as seconds
		{"", 0, false},
		{"soon", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			var v struct {
				D Duration `yaml:"d"`
			}
			err := unmarshalStrict([]byte("d: \""+tc.in+"\"\n"), &v)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q: want an error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if v.D.Duration() != tc.want {
				t.Errorf("%q = %s, want %s", tc.in, v.D, tc.want)
			}
		})
	}
}

func TestSiteAddresses(t *testing.T) {
	tests := []struct {
		name string
		e    Expose
		want []string
	}{
		{"bare domain", Expose{Domains: []string{"a.com"}}, []string{"a.com"}},
		{"with path", Expose{Domains: []string{"a.com"}, Path: "/api/*"}, []string{"a.com/api"}},
		{"multi domain", Expose{Domains: []string{"a.com", "b.com"}}, []string{"a.com", "b.com"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.e.SiteAddresses()
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestEnvKeysSorted(t *testing.T) {
	s := &Service{Env: map[string]string{"ZED": "1", "ALPHA": "2", "MID": "3"}}
	got := s.EnvKeys()
	want := []string{"ALPHA", "MID", "ZED"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EnvKeys = %v, want %v", got, want)
		}
	}
}

func TestMissingFleetFile(t *testing.T) {
	_, _, err := Load(t.TempDir())
	if err == nil {
		t.Fatal("want an error when fleet.yaml is absent")
	}
	if !strings.Contains(err.Error(), "pilot init") {
		t.Errorf("error should point at the fix, got: %v", err)
	}
}

// TestValidateUnit covers the systemd block. Every case here is a mistake that
// would otherwise surface mid-deploy, on the host, with the service stopped.
func TestValidateUnit(t *testing.T) {
	svc := func(unit string) string {
		return "name: x\nruntime: systemd\nhosts: [web-1]\nunit:\n" + unit
	}

	tests := []struct {
		name  string
		unit  string
		field string
		want  string
	}{
		{"bare unit name", "  name: hopboxd\n", "unit.name", "no unit suffix"},
		{"unknown kind", "  name: x.service\n  kind: timer\n", "unit.kind", "unknown kind"},
		{"timer on a service", "  name: x.service\n  timer: x.timer\n", "unit.timer", "only valid for `kind: oneshot`"},
		{"fresh on a service", "  name: x.service\n  fresh: 1h\n", "unit.fresh", "only valid for `kind: oneshot`"},
		{"relative link destination", "  name: x.service\n  links:\n    bin/x: x\n", "unit.links[bin/x]", "must be an absolute path"},
		{"absolute link source", "  name: x.service\n  links:\n    /usr/local/bin/x: /opt/x\n", "unit.links[/usr/local/bin/x]", "must be relative"},
		{"escaping link source", "  name: x.service\n  links:\n    /usr/local/bin/x: ../../etc/passwd\n", "unit.links[/usr/local/bin/x]", "escapes the release directory"},
		{"empty link source", "  name: x.service\n  links:\n    /usr/local/bin/x: \"\"\n", "unit.links[/usr/local/bin/x]", "missing the path"},
		{"empty precheck arg", "  name: x.service\n  precheck: [\"./x\", \"  \"]\n", "unit.precheck[1]", "empty argument"},

		// A oneshot with no timer is run by nothing, and one with no
		// freshness bound reports healthy forever after it stops working.
		// Both are warnings rather than errors: the config is legal, it just
		// silently does not do what the author meant.
		{"oneshot without timer", "  name: x.service\n  kind: oneshot\n  fresh: 48h\n", "unit.timer", "never scheduled"},
		{"oneshot without fresh", "  name: x.service\n  kind: oneshot\n  timer: x.timer\n", "unit.fresh", "still reports healthy"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := load(t, baseFleet, map[string]string{"x.yaml": svc(tc.unit)})
			if findDiag(ds, tc.field, tc.want) == nil {
				t.Errorf("want %s: %q; got:\n%v", tc.field, tc.want, ds.Sorted())
			}
		})
	}
}

// TestValidateUnitAccepted is the other half: a well-formed unit of each kind
// must produce no errors at all. Without this, a validator that rejected
// everything would pass every test above.
func TestValidateUnitAccepted(t *testing.T) {
	for name, unit := range map[string]string{
		"daemon":  "  name: hopboxd.service\n  links:\n    /usr/local/bin/hopboxd: hopboxd\n  precheck: [\"./hopboxd\", \"--check\"]\n",
		"oneshot": "  name: backup.service\n  kind: oneshot\n  timer: backup.timer\n  fresh: 48h\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, ds := load(t, baseFleet, map[string]string{
				"x.yaml": "name: x\nruntime: systemd\nhosts: [web-1]\nunit:\n" + unit,
			})
			assertNoErrors(t, ds)
		})
	}
}

// public_address takes both the scalar and the list form, because a
// single-address host is the common case and shouldn't need list syntax.
func TestHostPublicAddressAndBind(t *testing.T) {
	f, ds := load(t, `
version: 1
hosts:
  ks:
    address: ks
    public_address: 37.187.24.219
    caddy:
      bind: [37.187.24.219, "2001:41d0:a:18db::1"]
`, nil)
	assertNoErrors(t, ds)

	h := f.Hosts["ks"]
	if len(h.PublicAddress) != 1 || h.PublicAddress[0] != "37.187.24.219" {
		t.Errorf("scalar public_address = %v, want a one-element list", h.PublicAddress)
	}
	if len(h.Caddy.Bind) != 2 {
		t.Errorf("caddy.bind = %v, want both addresses", h.Caddy.Bind)
	}

	if got := f.CaddyBindFor([]string{"ks"}); len(got) != 2 {
		t.Errorf("CaddyBindFor(ks) = %v", got)
	}
	if got := f.CaddyBindFor([]string{"nope"}); got != nil {
		t.Errorf("CaddyBindFor of an unknown host = %v, want nil", got)
	}
}

// Both fields end up compared against DNS answers or written verbatim into
// generated Caddyfile blocks, so anything but a literal IP is an error.
func TestHostAddressFieldsRejectNonIPs(t *testing.T) {
	_, ds := load(t, `
version: 1
hosts:
  ks:
    address: ks
    public_address: [ks.example.com]
    caddy: {bind: ["not an ip"]}
`, nil)

	if findDiag(ds, "hosts.ks.public_address[0]", "not an IP") == nil {
		t.Errorf("hostname should be rejected as public_address:\n%v", ds.Sorted())
	}
	if findDiag(ds, "hosts.ks.caddy.bind[0]", "not an IP") == nil {
		t.Errorf("garbage should be rejected as caddy.bind:\n%v", ds.Sorted())
	}
}

// Bind addresses are host-local IPs, and an exposed service shares one
// rendered route across all its hosts — so caddy.bind on a multi-host
// service's host can never be satisfied and is rejected outright.
func TestExposedMultiHostServiceRejectsBind(t *testing.T) {
	fleet := `
version: 1
hosts:
  web-1:
    address: web1.example.com
    caddy: {bind: [203.0.113.7]}
  web-2:
    address: web2.example.com
`
	svc := `
name: api
runtime: compose
hosts: [web-1, web-2]
compose: {file: deploy/compose.yaml}
expose:
  domains: [api.example.com]
  upstream: 8080
`
	_, ds := load(t, fleet, map[string]string{"api.yaml": svc})
	d := findDiag(ds, "hosts", "bind addresses are host-local")
	if d == nil {
		t.Fatalf("want a host-local bind error:\n%v", ds.Sorted())
	}
	if !strings.Contains(d.Hint, "default_bind") {
		t.Errorf("the hint should name the mechanism that does work: %s", d.Hint)
	}

	// The same service on the binding host alone is fine.
	single := strings.ReplaceAll(svc, "hosts: [web-1, web-2]", "hosts: [web-1]")
	_, ds = load(t, fleet, map[string]string{"api.yaml": single})
	assertNoErrors(t, ds)
}
