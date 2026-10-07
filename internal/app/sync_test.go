package app

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
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
	cmds    []string
	scripts []string
	files   map[string][]byte
}

func newHostFake() *hostFake { return &hostFake{files: map[string][]byte{}} }

func (h *hostFake) Run(_ context.Context, cmd string) (transport.Result, error) {
	h.cmds = append(h.cmds, cmd)
	switch {
	case cmd == "systemctl --version":
		return transport.Result{Stdout: "systemd 252\n"}, nil
	case strings.HasPrefix(cmd, "systemctl is-active"):
		return transport.Result{Stdout: "active\n"}, nil
	case strings.HasPrefix(cmd, "test -e"):
		path := strings.Trim(strings.TrimPrefix(cmd, "test -e "), "'")
		if _, ok := h.files[path]; !ok {
			return transport.Result{ExitCode: 1}, nil
		}
	}
	return transport.Result{}, nil
}

func (h *hostFake) RunScript(_ context.Context, body string) (transport.Result, error) {
	h.scripts = append(h.scripts, body)
	if strings.Contains(body, "echo removed") {
		return transport.Result{Stdout: "removed\n"}, nil
	}
	return transport.Result{}, nil
}

func (h *hostFake) ReadFile(_ context.Context, p string) ([]byte, error) {
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
	a := statusApp(t)
	a.Layout = release.NewLayout("")
	route := path.Join(a.CaddyPaths().SnippetDir, "_status.caddy")

	var logged []string
	logf := func(f string, args ...any) { logged = append(logged, fmt.Sprintf(f, args...)) }

	web := newHostFake()
	web.files[route] = []byte("old")
	if err := a.syncStatusServer(context.Background(), web, "web-1", logf); err != nil {
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
	if err := a.syncStatusServer(context.Background(), box, "box-1", logf); err != nil {
		t.Fatal(err)
	}
	if !box.did("systemctl restart pilot-server.service") || box.did("disable --now pilot-server") {
		t.Errorf("box-1 should run the server:\n%s", strings.Join(append(box.cmds, box.scripts...), "\n"))
	}

	a.Fleet.Status = nil
	box = newHostFake()
	if err := a.syncStatusServer(context.Background(), box, "box-1", logf); err != nil {
		t.Fatal(err)
	}
	if !box.did("disable --now pilot-server") {
		t.Error("with no status block, the old status host kept its server")
	}
}
