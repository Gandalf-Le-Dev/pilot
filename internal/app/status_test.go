package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/server"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

const statusSecretValue = "fleet-secret-for-tests"

// statusApp loads a fleet where box-1 serves the page, web-1 runs a public
// site, and box-1 also runs a restricted admin and a hidden wiki.
func statusApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("PILOT_TEST_STATUS_SECRET", statusSecretValue)
	t.Setenv("PILOT_TEST_HOOK", "https://discord.example/api/webhooks/1/hook-token")

	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("fleet.yaml", `
version: 1
hosts:
  web-1: {address: web1.example.com}
  box-1: {address: box1.example.com}
  quiet: {address: quiet.example.com}
notifiers:
  discord: {type: discord, url: "${env:PILOT_TEST_HOOK}"}
  phone: {type: ntfy, url: "https://ntfy.example/private-topic"}
status:
  domain: status.example.com
  host: box-1
  listen: 100.64.0.10
  secret: ${env:PILOT_TEST_STATUS_SECRET}
  hide: [wiki]
  labels: {site: Website}
  notify: [discord]
`)
	svc := func(name, host, port, extra string) string {
		return "name: " + name + "\nruntime: compose\nhosts: [" + host + "]\ncompose: {file: c.yaml}\n" +
			"health: {docker: true}\nexpose: {domains: [" + name + ".example.com], upstream: " + port + extra + "}\n"
	}
	write("services/site.yaml", svc("site", "web-1", "8080", ""))
	write("services/admin.yaml", svc("admin", "box-1", "8081", ", allow: [100.64.0.0/10]"))
	write("services/wiki.yaml", svc("wiki", "box-1", "8082", ""))
	write("services/notes.yaml", svc("notes", "box-1", "8083", ""))

	f, ds, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if ds.HasErrors() {
		t.Fatalf("fixture is invalid: %v", ds.Sorted())
	}
	return &App{Root: root, Fleet: f}
}

func reportOf(t *testing.T, a *App, host string) *config.ReportTarget {
	t.Helper()
	spec, err := a.FleetConfigSpec(host)
	if err != nil {
		t.Fatal(err)
	}
	var fc config.FleetConfig
	if err := config.UnmarshalStrict([]byte(spec), &fc); err != nil {
		t.Fatalf("the agent would reject this spec: %v", err)
	}
	if fc.Report == nil {
		t.Fatalf("no report target for %s:\n%s", host, spec)
	}
	return fc.Report
}

// A host's token is derived from the secret and its own name, and the server
// can check it against the hash it was given without holding the token.
func TestStatusTokensAreDerivedPerHost(t *testing.T) {
	a := statusApp(t)
	web := reportOf(t, a, "web-1")
	if web.Token != statuspage.Token(statusSecretValue, "web-1") {
		t.Errorf("web-1 token = %q, want HMAC(secret, web-1)", web.Token)
	}

	raw, err := a.StatusServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := server.ParseConfig(raw)
	if err != nil {
		t.Fatalf("the server would reject its own config: %v\n%s", err, raw)
	}
	if got := cfg.Hosts["web-1"].TokenSHA256; got != statuspage.TokenHash(web.Token) {
		t.Errorf("stored hash %q does not match web-1's token", got)
	}
	if strings.Contains(string(raw), web.Token) || strings.Contains(string(raw), statusSecretValue) {
		t.Errorf("the server config carries a token or the secret:\n%s", raw)
	}
	if _, ok := cfg.Hosts["quiet"]; !ok {
		t.Error("a host with nothing on the page must still be watched for silence")
	}
}

// A restricted or hidden service never reaches an agent's list or the
// server's, whatever else is configured.
func TestStatusListsExcludeRestrictedAndHidden(t *testing.T) {
	a := statusApp(t)
	if got := strings.Join(reportOf(t, a, "box-1").Services, ","); got != "notes" {
		t.Errorf("box-1 reports %q, want notes alone (admin restricted, wiki hidden)", got)
	}
	if got := strings.Join(reportOf(t, a, "web-1").Services, ","); got != "site" {
		t.Errorf("web-1 reports %q, want site", got)
	}

	raw, _ := a.StatusServerConfig()
	cfg, _ := server.ParseConfig(raw)
	if got := cfg.Hosts["box-1"].Services; len(got) != 1 || got["notes"] != "notes" {
		t.Errorf("box-1 services on the server = %v", got)
	}
	if got := cfg.Hosts["web-1"].Services["site"]; got != "Website" {
		t.Errorf("site label = %q, want Website", got)
	}
	for _, leak := range []string{"admin", "wiki"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the server config names %s:\n%s", leak, raw)
		}
	}
}

// A host other than the server gets where to post and its own token, and
// nothing of the server's: no other host's token, no hash, no notifier the
// page was not asked to use.
func TestNonServerHostGetsOnlyItsURLAndToken(t *testing.T) {
	a := statusApp(t)
	spec, err := a.FleetConfigSpec("web-1")
	if err != nil {
		t.Fatal(err)
	}
	rep := reportOf(t, a, "web-1")
	if rep.URL != "http://100.64.0.10:7381/v1/report" {
		t.Errorf("web-1 posts to %q, want the tailnet listen address", rep.URL)
	}
	for _, other := range []string{"box-1", "quiet"} {
		if strings.Contains(spec, statuspage.Token(statusSecretValue, other)) {
			t.Errorf("web-1's config carries %s's token", other)
		}
	}
	if strings.Contains(spec, "token_sha256") || strings.Contains(spec, statusSecretValue) {
		t.Errorf("web-1's config carries server material:\n%s", spec)
	}

	if got := reportOf(t, a, "box-1").URL; got != "http://127.0.0.1:7381/v1/report" {
		t.Errorf("the status host posts to %q, want loopback", got)
	}
}

func TestServerConfigCarriesOnlyNamedNotifiersResolved(t *testing.T) {
	a := statusApp(t)
	raw, err := a.StatusServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	var cfg server.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notifiers) != 1 || cfg.Notifiers["discord"].Endpoint() != "https://discord.example/api/webhooks/1/hook-token" {
		t.Errorf("notifiers = %+v, want discord alone, resolved", cfg.Notifiers)
	}
}

func TestStatusRouteRendersForTheStatusHost(t *testing.T) {
	a := statusApp(t)
	route, err := server.Route(a.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"status.example.com {", "reverse_proxy 127.0.0.1:7380", "status block in fleet.yaml"} {
		if !strings.Contains(route, want) {
			t.Errorf("route lacks %q:\n%s", want, route)
		}
	}
	if strings.Contains(route, "services/_status.yaml") {
		t.Errorf("route points at a service file that does not exist:\n%s", route)
	}
}
