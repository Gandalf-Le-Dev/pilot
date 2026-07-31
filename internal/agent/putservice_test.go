package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

func putAgent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{
		Layout:   release.NewLayout(t.TempDir()),
		Host:     "web-1",
		services: map[string]*config.Service{},
		drift:    map[string]*driftRecord{},
		jobs:     NewJobStore(),
		alerts:   alert.NewEngine("web-1", nil),
	}
}

const bgSpec = "name: kite\nruntime: compose\nhosts: [web-1]\ncompose:\n  file: compose.yaml\n" +
	"rollout:\n  strategy: blue-green\n  service: kite\n  ports: [1, 2]\n"

const recreateSpec = "name: kite\nruntime: compose\nhosts: [web-1]\ncompose:\n  file: compose.yaml\n" +
	"rollout:\n  strategy: recreate\n"

// TestPutServiceRefreshesTheCachedSpec is the fix for a bug found by breaking a
// real fleet on purpose.
//
// A service was reverted from blue-green to recreate. `rollout` is not part of
// the release hash, so the deploy was a no-op and the spec never reached the
// agent — which went on looking for a compose project that did not exist and
// reported a service serving traffic as stopped.
func TestPutServiceRefreshesTheCachedSpec(t *testing.T) {
	a := putAgent(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	if _, err := a.PutService(bgSpec); err != nil {
		t.Fatal(err)
	}
	if got := a.services["kite"].Rollout.Strategy; got != "blue-green" {
		t.Fatalf("setup: strategy = %q", got)
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut,
		srv.URL+proto.PathServices+"kite", strings.NewReader(recreateSpec))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %s, want 204", resp.Status)
	}
	if got := a.services["kite"].Rollout.Strategy; got != "recreate" {
		t.Errorf("strategy = %q, want the pushed spec to have replaced it", got)
	}
}

// TestPutServiceRejectsAMismatchedName stops a spec landing under the wrong key,
// which would leave two services quietly describing one another.
func TestPutServiceRejectsAMismatchedName(t *testing.T) {
	a := putAgent(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut,
		srv.URL+proto.PathServices+"wakapi", strings.NewReader(recreateSpec))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400 for a spec pushed under another name", resp.Status)
	}
}

// TestPutServiceRejectsAnUnknownField — the same strict parsing a deploy uses.
// Silently ignoring a field would mean the agent observing with settings it
// only half understood.
func TestPutServiceRejectsAnUnknownField(t *testing.T) {
	a := putAgent(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut,
		srv.URL+proto.PathServices+"kite", strings.NewReader(recreateSpec+"nonsense: true\n"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
}
