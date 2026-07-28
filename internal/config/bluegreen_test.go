package config

import (
	"testing"
	"time"
)

const bgFleet = `
version: 1
hosts:
  web-1: {address: web1.example.com, tags: [prod]}
  web-2: {address: web2.example.com, tags: [prod]}
`

func bgService(extra string) string {
	return `
name: api
runtime: compose
hosts: [web-1]
compose: {file: compose.yaml}
expose:
  domains: [api.example.com]
  upstream: 8080
health: {http: {url: "http://localhost:8080/healthz"}}
rollout:
` + extra
}

func TestBlueGreenValidConfig(t *testing.T) {
	f, ds := load(t, bgFleet, map[string]string{
		"api.yaml": bgService("  strategy: blue-green\n  service: web\n  ports: [18080, 18081]\n  drain: 20s\n"),
	})
	assertNoErrors(t, ds)

	r := f.Services["api"].Rollout
	if !r.IsBlueGreen() {
		t.Fatal("strategy not recognised")
	}
	if r.PortFor(ColorBlue) != 18080 || r.PortFor(ColorGreen) != 18081 {
		t.Errorf("ports = %v", r.Ports)
	}
	if r.Drain.Duration() != 20*time.Second {
		t.Errorf("drain = %s", r.Drain)
	}
}

func TestBlueGreenDrainDefaults(t *testing.T) {
	f, ds := load(t, bgFleet, map[string]string{
		"api.yaml": bgService("  strategy: blue-green\n  service: web\n  ports: [18080, 18081]\n"),
	})
	if got := f.Services["api"].Rollout.DrainOrDefault(); got != DefaultDrain {
		t.Errorf("drain = %s, want defaulted %s", got, DefaultDrain)
	}
	// Defaulted, but the operator is told, because both versions serve during
	// that window and that is a real constraint on their app.
	if findDiag(ds, "rollout.drain", "not set") == nil {
		t.Errorf("want a warning that drain was defaulted; got:\n%v", ds.Sorted())
	}
}

func TestBlueGreenRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		rollout string
		field   string
		want    string
	}{
		{
			"no service named",
			"  strategy: blue-green\n  ports: [18080, 18081]\n",
			"rollout.service", "missing",
		},
		{
			"no ports",
			"  strategy: blue-green\n  service: web\n",
			"rollout.ports", "missing",
		},
		{
			"one port",
			"  strategy: blue-green\n  service: web\n  ports: [18080]\n",
			"rollout.ports", "want exactly 2",
		},
		{
			"three ports",
			"  strategy: blue-green\n  service: web\n  ports: [1, 2, 3]\n",
			"rollout.ports", "want exactly 2",
		},
		{
			"both colors on one port",
			"  strategy: blue-green\n  service: web\n  ports: [18080, 18080]\n",
			"rollout.ports", "both colors would bind",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := load(t, bgFleet, map[string]string{"api.yaml": bgService(tc.rollout)})
			if findDiag(ds, tc.field, tc.want) == nil {
				t.Errorf("want %s: %q; got:\n%v", tc.field, tc.want, ds.Sorted())
			}
		})
	}
}

// Under blue-green, expose.upstream is the *container* port and rollout.ports
// are the host ports. Conflating them is the easy mistake, so name it.
func TestBlueGreenRejectsUpstreamReusedAsHostPort(t *testing.T) {
	_, ds := load(t, bgFleet, map[string]string{
		"api.yaml": bgService("  strategy: blue-green\n  service: web\n  ports: [8080, 18081]\n"),
	})
	d := findDiag(ds, "rollout.ports[0]", "is also `expose.upstream`")
	if d == nil {
		t.Fatalf("want an error; got:\n%v", ds.Sorted())
	}
	if d.Hint == "" {
		t.Error("this one needs a hint explaining the two meanings")
	}
}

func TestBlueGreenOnlyForCompose(t *testing.T) {
	_, ds := load(t, bgFleet, map[string]string{
		"blog.yaml": `
name: blog
runtime: static
hosts: [web-1]
build: {command: b, output: [dist/]}
expose: {domains: [blog.example.com], static: true}
rollout:
  strategy: blue-green
  service: web
  ports: [18080, 18081]
`,
	})
	d := findDiag(ds, "rollout.strategy", "two live stacks")
	if d == nil {
		t.Fatalf("want an error; got:\n%v", ds.Sorted())
	}
	if d.Hint == "" || !contains(d.Hint, "already swap atomically") {
		t.Errorf("hint should explain static needs none of this: %q", d.Hint)
	}
}

func TestBlueGreenNeedsARouteToFlip(t *testing.T) {
	_, ds := load(t, bgFleet, map[string]string{
		"worker.yaml": `
name: worker
runtime: compose
hosts: [web-1]
compose: {file: compose.yaml}
health: {docker: true}
rollout:
  strategy: blue-green
  service: web
  ports: [18080, 18081]
`,
	})
	if findDiag(ds, "rollout.strategy", "needs a route to flip") == nil {
		t.Errorf("want an error; got:\n%v", ds.Sorted())
	}
}

// A silent port collision is an outage. Catching it at config-load time is the
// entire reason ports are declared rather than inferred.
func TestHostPortCollisions(t *testing.T) {
	t.Run("two blue-green services sharing a color port", func(t *testing.T) {
		_, ds := load(t, bgFleet, map[string]string{
			"api.yaml": bgService("  strategy: blue-green\n  service: web\n  ports: [18080, 18081]\n"),
			"web.yaml": `
name: web
runtime: compose
hosts: [web-1]
compose: {file: compose.yaml}
expose: {domains: [web.example.com], upstream: 3000}
health: {docker: true}
rollout:
  strategy: blue-green
  service: app
  ports: [18081, 18082]
`,
		})
		if findDiag(ds, "rollout.ports[1]", "also claimed by") == nil &&
			findDiag(ds, "rollout.ports[0]", "also claimed by") == nil {
			t.Errorf("want a collision error; got:\n%v", ds.Sorted())
		}
	})

	t.Run("recreate service colliding with a blue-green color", func(t *testing.T) {
		_, ds := load(t, bgFleet, map[string]string{
			"api.yaml": bgService("  strategy: blue-green\n  service: web\n  ports: [18080, 18081]\n"),
			"other.yaml": `
name: other
runtime: compose
hosts: [web-1]
compose: {file: compose.yaml}
expose: {domains: [other.example.com], upstream: 18080}
health: {docker: true}
`,
		})
		if findDiag(ds, "expose.upstream", "also claimed by") == nil {
			t.Errorf("want a collision error; got:\n%v", ds.Sorted())
		}
	})

	t.Run("same ports on different hosts do not collide", func(t *testing.T) {
		_, ds := load(t, bgFleet, map[string]string{
			"api.yaml": bgService("  strategy: blue-green\n  service: web\n  ports: [18080, 18081]\n"),
			"api2.yaml": `
name: api2
runtime: compose
hosts: [web-2]
compose: {file: compose.yaml}
expose: {domains: [api2.example.com], upstream: 8080}
health: {docker: true}
rollout:
  strategy: blue-green
  service: web
  ports: [18080, 18081]
`,
		})
		assertNoErrors(t, ds)
	})
}

func TestColorOther(t *testing.T) {
	if ColorBlue.Other() != ColorGreen || ColorGreen.Other() != ColorBlue {
		t.Error("colors should alternate")
	}
	// An unset color reads as blue, so a first deploy goes to green.
	if Color("").Other() != ColorGreen {
		t.Error("an unset color should behave as blue")
	}
}

func TestPortForWithoutPorts(t *testing.T) {
	r := &Rollout{Strategy: StrategyBlueGreen}
	if r.PortFor(ColorBlue) != 0 {
		t.Error("an unconfigured rollout should report no port rather than guess")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
