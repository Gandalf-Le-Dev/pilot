package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gandalfledev/pilot/internal/config"
	"github.com/gandalfledev/pilot/internal/release"
	"github.com/gandalfledev/pilot/internal/transport/proto"
)

// deployReq builds a minimal deploy request.
func deployReq(service, rel, spec string) proto.DeployRequest {
	return proto.DeployRequest{Service: service, Release: rel, Spec: spec, Verify: true}
}

// A blue-green health check must target the color under test, not the public
// URL. Probing the public URL would hit the color that is *already* live —
// which would pass, and would mean nothing.
func TestRewriteHealthPortTargetsTheColourUnderTest(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		h := &config.Health{HTTP: &config.HTTPProbe{URL: "http://localhost:8080/healthz", Expect: 200}}
		got := rewriteHealthPort(h, 18081)

		if got.HTTP.URL != "http://localhost:18081/healthz" {
			t.Errorf("URL = %q", got.HTTP.URL)
		}
		if got.HTTP.Expect != 200 {
			t.Error("rewriting the port should not disturb anything else")
		}
		// The original must not be mutated — it is shared with the cached spec.
		if h.HTTP.URL != "http://localhost:8080/healthz" {
			t.Errorf("original health check was mutated: %q", h.HTTP.URL)
		}
	})

	t.Run("https with a path preserved", func(t *testing.T) {
		h := &config.Health{HTTP: &config.HTTPProbe{URL: "https://api.example.com/v1/health"}}
		got := rewriteHealthPort(h, 18080)
		if got.HTTP.URL != "https://api.example.com:18080/v1/health" {
			t.Errorf("URL = %q", got.HTTP.URL)
		}
	})

	t.Run("tcp", func(t *testing.T) {
		h := &config.Health{TCP: &config.TCPProbe{Addr: "localhost:8080"}}
		got := rewriteHealthPort(h, 18081)
		if got.TCP.Addr != "127.0.0.1:18081" {
			t.Errorf("addr = %q", got.TCP.Addr)
		}
	})

	t.Run("docker health is already per-colour", func(t *testing.T) {
		h := &config.Health{Docker: true}
		got := rewriteHealthPort(h, 18081)
		if !got.Docker {
			t.Error("container-level health should be left alone")
		}
	})

	t.Run("no health check", func(t *testing.T) {
		if rewriteHealthPort(nil, 18080) != nil {
			t.Error("nil in, nil out")
		}
	})
}

func TestReplacePortLeavesUnparseableURLsAlone(t *testing.T) {
	if got := replacePort("://nonsense", 8080); got != "://nonsense" {
		t.Errorf("got %q; a URL we cannot parse should pass through unchanged", got)
	}
}

// The active colour is read from state.json, not inferred from the installed
// route, so a hand-edited route surfaces as drift rather than silently
// redirecting the next deploy.
func TestActiveColorFromState(t *testing.T) {
	a := newAgent(t)

	// With no state at all, blue is active, so a first deploy goes to green.
	if got := a.activeColor("api"); got != config.ColorBlue {
		t.Errorf("active = %q, want blue by default", got)
	}

	dir := a.Layout.Service("api")
	if err := release.EnsureService(dir); err != nil {
		t.Fatal(err)
	}
	a.setActiveColor("api", config.ColorGreen)

	if got := a.activeColor("api"); got != config.ColorGreen {
		t.Errorf("active = %q, want green", got)
	}
	if got := a.activeColor("api").Other(); got != config.ColorBlue {
		t.Errorf("next deploy would target %q, want blue", got)
	}

	st, err := release.ReadState(dir, "api")
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveColor != "green" {
		t.Errorf("state.json should record the colour: %+v", st)
	}
}

// The route Pilot installs must point at the colour's host port, while the
// document root still follows the release symlink.
func TestRenderColorRoute(t *testing.T) {
	a := newAgent(t)
	svc := &config.Service{
		Name:    "api",
		Runtime: config.RuntimeCompose,
		Expose:  &config.Expose{Domains: []string{"api.example.com"}, Upstream: 8080},
		Rollout: &config.Rollout{
			Strategy: config.StrategyBlueGreen,
			Service:  "web",
			Ports:    []int{18080, 18081},
		},
	}

	blue, err := a.renderColorRoute(svc, config.ColorBlue)
	if err != nil {
		t.Fatal(err)
	}
	green, err := a.renderColorRoute(svc, config.ColorGreen)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(blue, "reverse_proxy 127.0.0.1:18080") {
		t.Errorf("blue route:\n%s", blue)
	}
	if !strings.Contains(green, "reverse_proxy 127.0.0.1:18081") {
		t.Errorf("green route:\n%s", green)
	}
	// The container port is an implementation detail of the stack, not
	// something Caddy should ever be pointed at.
	if strings.Contains(blue, ":8080") {
		t.Errorf("route must use the host port, not the container port:\n%s", blue)
	}

	// The flip is exactly one line changing, which is why a Caddy reload is
	// cheap and why an unchanged deploy skips it entirely.
	if countDiffLines(blue, green) != 1 {
		t.Errorf("a flip should change one line:\nblue:\n%s\ngreen:\n%s", blue, green)
	}
}

func TestRenderColorRouteNeedsExpose(t *testing.T) {
	a := newAgent(t)
	svc := &config.Service{Name: "worker", Rollout: &config.Rollout{Ports: []int{1, 2}}}
	if _, err := a.renderColorRoute(svc, config.ColorBlue); err == nil {
		t.Error("a service with no route has nothing to flip")
	}
}

// A blue-green deploy against a service whose compose stack cannot start must
// leave the live colour untouched and say so.
func TestBlueGreenFailureLeavesActiveColourServing(t *testing.T) {
	a := newAgent(t)

	// docker is not available in the test environment, so StartColor fails —
	// which is exactly the "new colour never came up" path.
	spec := `
name: api
runtime: compose
hosts: [web-1]
compose: {file: compose.yaml}
expose: {domains: [api.example.com], upstream: 8080}
health: {tcp: {addr: "127.0.0.1:1"}, timeout: 1s, interval: 300ms}
rollout:
  strategy: blue-green
  service: web
  ports: [18080, 18081]
  drain: 1s
`
	stageRelease(t, a, "api", "0001-aaaaaaa", map[string]string{
		"compose.yaml": "services:\n  web:\n    image: nginx\n",
	})

	job, err := a.StartDeploy(deployReq("api", "0001-aaaaaaa", spec))
	if err != nil {
		t.Fatal(err)
	}
	final := waitDone(t, a, job.ID)

	if final.State != proto.JobFailed {
		t.Fatalf("expected failure, got %q\n%s", final.State, formatEvents(final))
	}
	events := formatEvents(final)
	if !strings.Contains(events, "no user impact") {
		t.Errorf("a pre-flip failure should say the live version is untouched:\n%s", events)
	}
	// Nothing was activated, so `current` must not exist.
	if _, err := a.Exec.Exists(t.Context(), a.Layout.Current("api")); err == nil {
		if ok, _ := a.Exec.Exists(t.Context(), a.Layout.Current("api")); ok {
			t.Error("a failed pre-flip deploy must not have moved the symlink")
		}
	}
}

func TestBlueGreenDispatch(t *testing.T) {
	a := newAgent(t)
	stageRelease(t, a, "api", "0001-aaaaaaa", map[string]string{"compose.yaml": "services: {}\n"})

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	if _, err := http.Get(srv.URL + "/v1/status"); err != nil {
		t.Fatal(err)
	}
}

func countDiffLines(a, b string) int {
	as, bs := strings.Split(a, "\n"), strings.Split(b, "\n")
	if len(as) != len(bs) {
		return -1
	}
	n := 0
	for i := range as {
		if as[i] != bs[i] {
			n++
		}
	}
	return n
}
