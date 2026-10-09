package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/server"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// statusHost fakes the status host: the unit's state, the page's JSON, and
// the files under the snippet directory.
type statusHost struct {
	active string
	page   string
	files  map[string][]byte
	cmds   []string
}

func (h *statusHost) Run(_ context.Context, cmd string) (transport.Result, error) {
	h.cmds = append(h.cmds, cmd)
	switch {
	case strings.HasPrefix(cmd, "systemctl is-active"):
		return transport.Result{Stdout: h.active + "\n"}, nil
	case strings.HasPrefix(cmd, "curl"):
		return transport.Result{Stdout: h.page}, nil
	}
	return transport.Result{}, nil
}

func (h *statusHost) RunScript(ctx context.Context, body string) (transport.Result, error) {
	return h.Run(ctx, body)
}

func (h *statusHost) ReadFile(_ context.Context, p string) ([]byte, error) {
	if b, ok := h.files[p]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

func (h *statusHost) WriteFile(_ context.Context, p string, data []byte, _ string) error {
	h.files[p] = append([]byte(nil), data...)
	return nil
}

var statusPaths = caddy.Paths{Caddyfile: "/etc/caddy/Caddyfile", SnippetDir: "/etc/caddy/pilot.d"}

func statusFleet() *config.Fleet {
	return &config.Fleet{
		Hosts: map[string]*config.Host{"ks": {Name: "ks"}, "vps": {Name: "vps"}},
		Services: map[string]*config.Service{
			"docmost": {Name: "docmost", Hosts: []string{"vps"}, Expose: &config.Expose{Domains: []string{"notes.example.com"}, Upstream: 3000}},
			"site":    {Name: "site", Hosts: []string{"ks"}, Expose: &config.Expose{Domains: []string{"example.com"}, Upstream: 8080}},
		},
		Status: &config.Status{Domain: "status.example.com", Host: "ks", Labels: map[string]string{"docmost": "Notes"}},
	}
}

func pageJSON(t *testing.T, services ...statuspage.Service) string {
	t.Helper()
	b, err := json.Marshal(statuspage.Page{Title: "t", Services: services})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func healthyStatusHost(t *testing.T, f *config.Fleet) *statusHost {
	t.Helper()
	route, err := server.Route(f)
	if err != nil {
		t.Fatal(err)
	}
	return &statusHost{
		active: "active",
		page:   pageJSON(t, statuspage.Service{Label: "Notes", State: statuspage.Up}),
		files:  map[string][]byte{path.Join(statusPaths.SnippetDir, "_status.caddy"): []byte(route)},
	}
}

func TestStatusServerHealthy(t *testing.T) {
	f := statusFleet()
	got := inspectStatusServer(context.Background(), healthyStatusHost(t, f), f, statusPaths)
	if len(got) != 1 || got[0].Status != StatusOK {
		t.Errorf("findings = %+v, want one ok", got)
	}
}

func TestStatusServerNotRunning(t *testing.T) {
	f := statusFleet()
	h := healthyStatusHost(t, f)
	h.active = "failed"
	got := inspectStatusServer(context.Background(), h, f, statusPaths)
	if len(got) != 1 || got[0].Status != StatusFail || !strings.Contains(got[0].Title, "failed") {
		t.Fatalf("findings = %+v, want the unit's state as a failure", got)
	}
	for _, cmd := range h.cmds {
		if strings.HasPrefix(cmd, "curl") {
			t.Error("read the page of a server that is not running")
		}
	}
}

// The page names labels, never hosts; doctor names the hosts, because the
// operator needs to know which machine to go and look at.
func TestStatusServerNamesSilentHosts(t *testing.T) {
	f := statusFleet()
	h := healthyStatusHost(t, f)
	h.page = pageJSON(t,
		statuspage.Service{Label: "Notes", State: statuspage.NotReporting, NotReportingSince: time.Now()},
		statuspage.Service{Label: "site", State: statuspage.Up},
	)
	got := inspectStatusServer(context.Background(), h, f, statusPaths)
	if len(got) != 1 || got[0].Status != StatusFail || !strings.HasSuffix(got[0].Title, ": vps") {
		t.Errorf("findings = %+v, want vps named as not reporting", got)
	}
}

func TestStatusRouteDriftIsFixed(t *testing.T) {
	f := statusFleet()
	h := healthyStatusHost(t, f)
	target := path.Join(statusPaths.SnippetDir, "_status.caddy")
	h.files[target] = []byte("status.example.com {\n\trespond \"hand-edited\"\n}\n")

	got := inspectStatusServer(context.Background(), h, f, statusPaths)
	var drift *Finding
	for i := range got {
		if strings.Contains(got[i].Title, "route differs") {
			drift = &got[i]
		}
	}
	if drift == nil || !drift.Fixable() {
		t.Fatalf("findings = %+v, want a fixable route drift", got)
	}
	if err := drift.Fix(context.Background()); err != nil {
		t.Fatal(err)
	}
	want, _ := server.Route(f)
	if string(h.files[target]) != want {
		t.Errorf("after the fix the route is:\n%s", h.files[target])
	}

	delete(h.files, target)
	got = inspectStatusServer(context.Background(), h, f, statusPaths)
	if got[len(got)-1].Title != "status page route missing: _status.caddy" {
		t.Errorf("a missing route was not reported: %+v", got)
	}
}

// The status domain is checked like any exposed domain, against the public
// address of the host that serves it.
func TestEdgeCoversTheStatusDomain(t *testing.T) {
	sites := edgeSites(statusFleet())
	last := sites[len(sites)-1]
	if last.Expose.Domains[0] != "status.example.com" || last.Hosts[0] != "ks" {
		t.Errorf("edge sites end with %+v, want the status domain on ks", last)
	}
	f := statusFleet()
	f.Status = nil
	if n := len(edgeSites(f)); n != 2 {
		t.Errorf("without a status block there are %d sites, want the 2 services", n)
	}
}

// A server on any host but status.host is found, also when the fleet has
// no status page any more, and --fix removes it.
func TestStrayStatusServerIsFoundAndRemoved(t *testing.T) {
	f := statusFleet()
	h := &statusHost{active: "active", files: map[string][]byte{}}

	got := strayStatusServer(context.Background(), h, f, "vps")
	if got == nil || got.Status != StatusFail || !strings.Contains(got.Title, "status.host is ks") {
		t.Fatalf("finding = %+v, want a failure naming the real status host", got)
	}
	if err := got.Fix(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(h.cmds, "\n"), "systemctl disable --now pilot-server.service") {
		t.Errorf("the fix did not stop the server:\n%s", strings.Join(h.cmds, "\n"))
	}

	f.Status = nil
	h.active = "failed"
	got = strayStatusServer(context.Background(), h, f, "ks")
	if got == nil || !strings.Contains(got.Title, "no status page") {
		t.Errorf("finding = %+v, want a crash-looping leftover reported with no status block", got)
	}

	h.active = "inactive"
	if got := strayStatusServer(context.Background(), h, f, "ks"); got != nil {
		t.Errorf("a host with no server was reported: %+v", got)
	}
}
