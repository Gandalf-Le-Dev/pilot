package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/install"
	"github.com/Gandalf-Le-Dev/pilot/internal/agent/remote"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

func TestStatusHostSyncsLast(t *testing.T) {
	a := statusApp(t)
	got := a.StatusHostLast([]string{"box-1", "quiet", "web-1"})
	if strings.Join(got, ",") != "quiet,web-1,box-1" {
		t.Errorf("order = %v, want box-1 last", got)
	}
	if got := a.StatusHostLast([]string{"web-1"}); strings.Join(got, ",") != "web-1" {
		t.Errorf("a selection without the status host changed: %v", got)
	}
	a.Fleet.Status = nil
	if got := a.StatusHostLast([]string{"box-1", "web-1"}); strings.Join(got, ",") != "box-1,web-1" {
		t.Errorf("without a status page the order changed: %v", got)
	}
}

// hostFake records what a sync does to one host. Methods the status sync
// does not use are left to the embedded nil interface, so reaching one fails
// the test loudly.
type hostFake struct {
	transport.Executor
	cmds     []string
	scripts  []string
	files    map[string][]byte
	inputs   [][]byte
	reads    []string
	serverUp bool

	// serverFails keeps the unit down after a restart, as a server that
	// rejects its configuration would.
	serverFails bool
}

func newHostFake() *hostFake { return &hostFake{files: map[string][]byte{}} }

func (h *hostFake) Run(_ context.Context, cmd string) (transport.Result, error) {
	h.cmds = append(h.cmds, cmd)
	switch {
	case cmd == "systemctl --version":
		return transport.Result{Stdout: "systemd 252\n"}, nil
	case strings.HasPrefix(cmd, "systemctl is-active"):
		if !h.serverUp {
			return transport.Result{Stdout: "inactive\n", ExitCode: 3}, nil
		}
		return transport.Result{Stdout: "active\n"}, nil
	case strings.HasPrefix(cmd, "sha256sum "):
		path := strings.TrimPrefix(cmd, "sha256sum ")
		b, ok := h.files[path]
		if !ok {
			return transport.Result{ExitCode: 1, Stderr: "No such file or directory"}, nil
		}
		sum := sha256.Sum256(b)
		return transport.Result{Stdout: hex.EncodeToString(sum[:]) + "  " + path + "\n"}, nil
	case strings.Contains(cmd, "for f in *.caddy"):
		dir := strings.Trim(strings.Fields(cmd)[2], "'")
		var routes []string
		for p := range h.files {
			if rel, ok := strings.CutPrefix(p, dir+"/"); ok && strings.HasSuffix(rel, ".caddy") {
				routes = append(routes, rel)
			}
		}
		return transport.Result{Stdout: strings.Join(routes, "\n")}, nil
	}
	return transport.Result{}, nil
}

func (h *hostFake) RunScript(_ context.Context, body string) (transport.Result, error) {
	h.scripts = append(h.scripts, body)
	if strings.Contains(body, "systemctl restart pilot-server.service") {
		h.serverUp = !h.serverFails
	}
	if strings.Contains(body, "echo removed") {
		return transport.Result{Stdout: "removed\n"}, nil
	}
	return transport.Result{}, nil
}

func (h *hostFake) RunInput(_ context.Context, cmd string, stdin []byte) (transport.Result, error) {
	h.cmds = append(h.cmds, cmd)
	h.inputs = append(h.inputs, stdin)
	return transport.Result{}, nil
}

// pushedDigest is the digest stamped on the last config pushed to the agent.
func (h *hostFake) pushedDigest(t *testing.T) string {
	t.Helper()
	if len(h.inputs) == 0 {
		t.Fatal("no config was pushed to the agent")
	}
	var req proto.ConfigRequest
	if err := json.Unmarshal(h.inputs[len(h.inputs)-1], &req); err != nil {
		t.Fatal(err)
	}
	return config.StampedDigest(req.Spec)
}

func (h *hostFake) count(sub string) int {
	n := 0
	for _, c := range append(h.cmds, h.scripts...) {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func (h *hostFake) ReadFile(_ context.Context, p string) ([]byte, error) {
	h.reads = append(h.reads, p)
	if b, ok := h.files[p]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

func (h *hostFake) WriteFile(_ context.Context, p string, data []byte, _ string) error {
	h.files[p] = append([]byte(nil), data...)
	return nil
}

func (h *hostFake) MkdirAll(context.Context, string) error { return nil }

func (h *hostFake) did(sub string) bool {
	return slices.ContainsFunc(append(h.cmds, h.scripts...), func(c string) bool { return strings.Contains(c, sub) })
}

// Every host other than status.host loses any status server and route it
// still carries, and so does every host once the status block is gone.
func TestSyncRemovesStatusServersOffTheStatusHost(t *testing.T) {
	fastSettle(t)
	a := statusApp(t)
	a.Layout = release.NewLayout("")
	route := path.Join(a.CaddyPaths().SnippetDir, "_status.caddy")

	var logged []string
	logf := func(f string, args ...any) { logged = append(logged, fmt.Sprintf(f, args...)) }

	web := newHostFake()
	web.files[route] = []byte("old")
	if _, err := a.syncStatusServer(context.Background(), web, "web-1", logf); err != nil {
		t.Fatal(err)
	}
	if !web.did("systemctl disable --now pilot-server.service") || !web.did("rm -f "+route) {
		t.Errorf("web-1 kept its status server or route:\n%s", strings.Join(append(web.cmds, web.scripts...), "\n"))
	}
	if web.did("systemctl restart pilot-server.service") {
		t.Error("a status server was started on a host that is not status.host")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "removed") {
		t.Errorf("logged %q, want the removal said out loud", logged)
	}

	box := newHostFake()
	if _, err := a.syncStatusServer(context.Background(), box, "box-1", logf); err != nil {
		t.Fatal(err)
	}
	if !box.did("systemctl restart pilot-server.service") || box.did("disable --now pilot-server") {
		t.Errorf("box-1 should run the server:\n%s", strings.Join(append(box.cmds, box.scripts...), "\n"))
	}

	a.Fleet.Status = nil
	box = newHostFake()
	if _, err := a.syncStatusServer(context.Background(), box, "box-1", logf); err != nil {
		t.Fatal(err)
	}
	if !box.did("disable --now pilot-server") {
		t.Error("with no status block, the old status host kept its server")
	}
}

func fastSettle(t *testing.T) {
	t.Helper()
	settle, poll := install.ServerSettle, install.ServerPoll
	install.ServerSettle, install.ServerPoll = 5*time.Millisecond, time.Millisecond
	t.Cleanup(func() { install.ServerSettle, install.ServerPoll = settle, poll })
}

// A host `pilot agent upgrade` skips as current still loses a stray server,
// and the status host gets one if it has none, but a running one is left
// alone: a fleet-wide upgrade that changes nothing must restart nothing.
func TestTidyStatusServer(t *testing.T) {
	fastSettle(t)
	a := statusApp(t)
	a.Layout = release.NewLayout("")
	logf := func(string, ...any) {}

	web := newHostFake()
	web.files[install.ServerConfigPath] = []byte("old")
	did, err := a.tidyStatusServer(context.Background(), web, "web-1", logf)
	if err != nil {
		t.Fatal(err)
	}
	if !did || !web.did("systemctl disable --now pilot-server.service") {
		t.Errorf("a current host off status.host kept its status server (changed = %v)", did)
	}

	box := newHostFake()
	box.serverUp = true
	if did, err := a.tidyStatusServer(context.Background(), box, "box-1", logf); err != nil || did {
		t.Fatalf("changed = %v, err = %v; want nothing done", did, err)
	}
	if box.did("systemctl restart pilot-server.service") || box.did("disable --now pilot-server") {
		t.Errorf("a running status server was touched:\n%s", strings.Join(append(box.cmds, box.scripts...), "\n"))
	}

	box = newHostFake()
	if did, err := a.tidyStatusServer(context.Background(), box, "box-1", logf); err != nil || !did {
		t.Fatalf("changed = %v, err = %v; want the server installed", did, err)
	}
	if !box.did("systemctl restart pilot-server.service") {
		t.Error("status.host moved to a current host, and it got no server")
	}
}

// A config push to the status host rewrites the server's configuration
// before the agent's, since the digest the agent then reports covers both.
func TestPushToTheStatusHostInstallsTheServerFirst(t *testing.T) {
	fastSettle(t)
	a := statusApp(t)
	a.Layout = release.NewLayout("")

	box := newHostFake()
	if err := a.pushHostConfig(context.Background(), box, remote.New(box, "box-1", a.Layout), "box-1"); err != nil {
		t.Fatal(err)
	}
	cfg := slices.IndexFunc(box.cmds, func(c string) bool { return strings.Contains(c, "ctl config") })
	restart := slices.IndexFunc(box.cmds, func(c string) bool { return strings.HasPrefix(c, "systemctl is-active pilot-server") })
	if _, ok := box.files[install.ServerConfigPath]; !ok || restart < 0 || cfg < restart {
		t.Errorf("want the server installed, then the config pushed:\n%s", strings.Join(box.cmds, "\n"))
	}

	web := newHostFake()
	if err := a.pushHostConfig(context.Background(), web, remote.New(web, "web-1", a.Layout), "web-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := web.files[install.ServerConfigPath]; ok || web.did("pilot-server") {
		t.Error("a config push gave a server to a host that is not status.host")
	}
}

// A status server that will not start must not cost the host its alerting:
// the agent still gets its config, stamped short of the server's half so
// doctor keeps calling the host stale and --fix tries the server again.
func TestPushToTheStatusHostSurvivesAFailedServer(t *testing.T) {
	fastSettle(t)
	a := statusApp(t)
	a.Layout = release.NewLayout("")

	box := newHostFake()
	box.serverFails = true
	err := a.pushHostConfig(context.Background(), box, remote.New(box, "box-1", a.Layout), "box-1")
	if err == nil || !strings.Contains(err.Error(), "status server") {
		t.Fatalf("err = %v, want the server's failure reported", err)
	}
	want, _ := a.FleetConfigDigest("box-1")
	if got := box.pushedDigest(t); got == "" || got == want {
		t.Errorf("pushed digest %q after a failed server install, want one that reads as stale (not %q)", got, want)
	}

	box.serverFails = false
	if err := a.pushHostConfig(context.Background(), box, remote.New(box, "box-1", a.Layout), "box-1"); err != nil {
		t.Fatal(err)
	}
	if got := box.pushedDigest(t); got != want {
		t.Errorf("pushed digest %q once the server started, want %q", got, want)
	}
}

// A push the server's configuration does not need leaves the running server
// alone: a restart drops every host's report and restarts the silence clock.
func TestPushRestartsTheStatusServerOnlyWhenItsConfigChanged(t *testing.T) {
	fastSettle(t)
	a := statusApp(t)
	a.Layout = release.NewLayout("")
	box := newHostFake()
	push := func() {
		t.Helper()
		if err := a.pushHostConfig(context.Background(), box, remote.New(box, "box-1", a.Layout), "box-1"); err != nil {
			t.Fatal(err)
		}
	}

	push()
	const restart = "systemctl restart pilot-server.service"
	if n := box.count(restart); n != 1 {
		t.Fatalf("%d restarts on the first push, want 1", n)
	}

	before, _ := a.FleetConfigDigest("box-1")
	off := false
	a.Fleet.NotifyDeploys = &off
	if after, _ := a.FleetConfigDigest("box-1"); after == before {
		t.Fatal("the agent-only change did not move the digest, so it proves nothing")
	}
	push()
	if n := box.count(restart); n != 1 {
		t.Errorf("an agent-only change restarted the status server (%d restarts)", n)
	}

	a.Fleet.Status.Labels = map[string]string{"site": "Site"}
	push()
	if n := box.count(restart); n != 2 {
		t.Errorf("a label change did not reach the status server (%d restarts)", n)
	}
	if slices.Contains(box.reads, install.ServerConfigPath) {
		t.Error("the installed server config, webhooks and all, was read back over SSH")
	}
}
