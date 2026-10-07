package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// receiver stands in for the status server and keeps what it was sent.
type receiver struct {
	mu     sync.Mutex
	auth   []string
	bodies [][]byte
	code   int
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.auth = append(rc.auth, r.Header.Get("Authorization"))
	rc.bodies = append(rc.bodies, body)
	code := rc.code
	rc.mu.Unlock()
	if code == 0 {
		code = http.StatusNoContent
	}
	w.WriteHeader(code)
}

func (rc *receiver) last(t *testing.T) (string, []byte) {
	t.Helper()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.bodies) == 0 {
		t.Fatal("nothing was posted")
	}
	return rc.auth[len(rc.auth)-1], rc.bodies[len(rc.bodies)-1]
}

// deployStatic activates a static service with an HTTP health check.
func deployStatic(t *testing.T, a *Agent, name, release, healthURL string) {
	t.Helper()
	stageRelease(t, a, name, release, map[string]string{"index.html": "<h1>" + name + "</h1>"})
	spec := strings.ReplaceAll(staticSpec, "name: blog", "name: "+name) +
		"health: {http: {url: \"" + healthURL + "\"}}\n"
	job, err := a.StartDeploy(proto.DeployRequest{Service: name, Release: release, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, a, job.ID)
}

// Only listed services travel, under this host's token, and nothing a
// runtime says about them does: not the probe URL, not the release, not the
// detail line explaining why a service is down.
func TestPushSendsOnlyListedServicesWithoutDetail(t *testing.T) {
	probe, _ := switchable(t)
	rc := &receiver{}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)

	a := newAgent(t)
	deployStatic(t, a, "blog", "0001-aaaaaaa", probe.URL)
	deployStatic(t, a, "admin", "0002-bbbbbbb", probe.URL)
	// Cached but never activated: its observation carries a Detail line.
	if _, err := a.PutService(strings.ReplaceAll(staticSpec, "name: blog", "name: ghost")); err != nil {
		t.Fatal(err)
	}

	target := &config.ReportTarget{URL: srv.URL + statuspage.ReportPath, Token: "tok-web-1", Services: []string{"blog", "ghost"}}
	a.sampleHealth(context.Background(), target.Services)
	if err := a.pushReport(context.Background(), target); err != nil {
		t.Fatal(err)
	}

	auth, body := rc.last(t)
	if auth != "Bearer tok-web-1" {
		t.Errorf("Authorization = %q", auth)
	}

	var rep statuspage.Report
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		t.Fatalf("the server would reject this report: %v\n%s", err, body)
	}
	if rep.Host != "web-1" {
		t.Errorf("host = %q", rep.Host)
	}
	var names []string
	for _, s := range rep.Services {
		names = append(names, s.Name+"="+string(s.State))
	}
	if got := strings.Join(names, ","); got != "blog=up,ghost=down" {
		t.Errorf("services = %s, want blog=up,ghost=down and no admin", got)
	}

	ghost, _ := a.Observe(context.Background(), "ghost")
	for _, leak := range []string{
		"admin", "0001-aaaaaaa", "0002-bbbbbbb", strings.TrimPrefix(probe.URL, "http://"),
		"127.0.0.1", "detail", ghost.Detail, "tok-web-1",
	} {
		if leak != "" && bytes.Contains(body, []byte(leak)) {
			t.Errorf("the report carries %q:\n%s", leak, body)
		}
	}
}

// A refused or hanging push returns within its timeout and the next tick
// tries again; nothing queues, and nothing else waits on it.
func TestPushFailureIsBoundedAndRetried(t *testing.T) {
	saved := pushTimeout
	pushTimeout = 100 * time.Millisecond
	t.Cleanup(func() { pushTimeout = saved })

	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); hanging.Close() })

	a := newAgent(t)
	start := time.Now()
	err := a.pushReport(context.Background(), &config.ReportTarget{URL: hanging.URL, Token: "t"})
	if err == nil {
		t.Fatal("a server that never answered was counted as a delivery")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("push blocked for %s despite a %s timeout", elapsed, pushTimeout)
	}

	rc := &receiver{code: http.StatusServiceUnavailable}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	target := &config.ReportTarget{URL: srv.URL, Token: "t"}
	if err := a.pushReport(context.Background(), target); err == nil {
		t.Error("a 503 was counted as a delivery")
	}
	rc.mu.Lock()
	rc.code = 0
	rc.mu.Unlock()
	if err := a.pushReport(context.Background(), target); err != nil {
		t.Errorf("the next tick did not recover: %v", err)
	}
}
