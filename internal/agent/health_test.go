package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// observedRuntime reports a fixed observation, standing in for an adapter.
type observedRuntime struct {
	runtime.Runtime
	obs runtime.Observation
	err error
}

func (r observedRuntime) Observe(context.Context, *runtime.Target) (runtime.Observation, error) {
	return r.obs, r.err
}

// switchable is an HTTP health endpoint whose answer a test can change.
func switchable(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	code := &atomic.Int32{}
	code.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(code.Load()))
	}))
	t.Cleanup(srv.Close)
	return srv, code
}

// The runtime speaks first, and the probe decides only for a running service.
func TestHealthOf(t *testing.T) {
	srv, code := switchable(t)
	target := &runtime.Target{Service: &config.Service{Name: "api", Health: &config.Health{
		HTTP: &config.HTTPProbe{URL: srv.URL, Expect: 200},
	}}}
	running := observedRuntime{obs: runtime.Observation{State: runtime.StateRunning}}

	if got := healthOf(context.Background(), running, target); got != statuspage.Up {
		t.Errorf("running and answering = %s, want up", got)
	}

	// The case the background loops never caught: the process is up, the
	// service is not.
	code.Store(http.StatusInternalServerError)
	if got := healthOf(context.Background(), running, target); got != statuspage.Down {
		t.Errorf("running with a failing probe = %s, want down", got)
	}

	for _, tc := range []struct {
		name string
		rt   observedRuntime
		want statuspage.State
	}{
		{"observe failed", observedRuntime{err: errors.New("docker: permission denied")}, statuspage.Unknown},
		{"runtime unsure", observedRuntime{obs: runtime.Observation{State: runtime.StateUnknown}}, statuspage.Unknown},
		{"degraded", observedRuntime{obs: runtime.Observation{State: runtime.StateDegraded}}, statuspage.Degraded},
		{"oneshot awaiting its run", observedRuntime{obs: runtime.Observation{State: runtime.StateDegraded, AwaitingRun: true}}, statuspage.Degraded},
		{"stopped", observedRuntime{obs: runtime.Observation{State: runtime.StateStopped}}, statuspage.Down},
		{"failed", observedRuntime{obs: runtime.Observation{State: runtime.StateFailed}}, statuspage.Down},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := healthOf(context.Background(), tc.rt, target); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// Buckets are indexed by time, so a gap reads as unknown rather than shifting
// later buckets, and the ring never holds more than a day.
func TestHealthRingBucketsByTime(t *testing.T) {
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var r healthRing

	r.record(base, statuspage.Up)
	r.record(base.Add(30*time.Second), statuspage.Down)
	r.record(base.Add(60*time.Second), statuspage.Up)
	// Nothing for the 12:05 bucket: the agent was restarting.
	r.record(base.Add(10*time.Minute), statuspage.Degraded)

	h := r.history(base.Add(12 * time.Minute))
	want := []statuspage.State{statuspage.Down, statuspage.Unknown, statuspage.Degraded}
	if !slices.Equal(h.States, want) {
		t.Errorf("states = %v, want %v (worst of each bucket, gap as unknown)", h.States, want)
	}
	if got := time.Unix(h.End, 0).UTC(); !got.Equal(base.Add(10 * time.Minute)) {
		t.Errorf("end = %s, want the newest bucket with a sample", got)
	}
	if r.current != statuspage.Degraded {
		t.Errorf("current = %s, want the latest sample", r.current)
	}

	// A bucket opened seconds ago and not yet sampled must not end the
	// history as unknown.
	h = r.history(base.Add(15*time.Minute + 5*time.Second))
	if last := h.States[len(h.States)-1]; last != statuspage.Degraded {
		t.Errorf("history ends with %s; an unsampled bucket should not be reported", last)
	}

	// A day and a half of samples keeps exactly one day.
	for i := range 3 * statuspage.HistoryBuckets / 2 {
		r.record(base.Add(time.Duration(i)*statuspage.BucketWidth), statuspage.Up)
	}
	end := base.Add(time.Duration(3*statuspage.HistoryBuckets/2-1) * statuspage.BucketWidth)
	h = r.history(end)
	if len(h.States) != statuspage.HistoryBuckets {
		t.Errorf("history holds %d buckets, want %d", len(h.States), statuspage.HistoryBuckets)
	}
	for _, s := range h.States {
		if s != statuspage.Up {
			t.Fatalf("a wrapped slot leaked into the history: %v", h.States)
		}
	}
}

// End to end on a real agent: the listed service is probed against an HTTP
// endpoint, sampled on a fake clock, and reported; nothing else is.
func TestSampleHealthProbesListedServices(t *testing.T) {
	srv, code := switchable(t)
	a := newAgent(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }

	stageRelease(t, a, "blog", "0001-aaaaaaa", map[string]string{"index.html": "<h1>v1</h1>"})
	spec := staticSpec + "health: {http: {url: \"" + srv.URL + "\"}}\n"
	job, err := a.StartDeploy(proto.DeployRequest{Service: "blog", Release: "0001-aaaaaaa", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, a, job.ID)

	if a.ReportTarget() != nil {
		t.Fatal("no status page configured, yet a report target exists")
	}
	if err := a.PutFleetConfig("report:\n  url: http://127.0.0.1:7381/v1/report\n  token: t\n  services: [blog, elsewhere]\n"); err != nil {
		t.Fatal(err)
	}
	listed := a.ReportTarget().Services

	a.sampleHealth(context.Background(), listed)
	code.Store(http.StatusBadGateway)
	now = now.Add(30 * time.Second)
	a.sampleHealth(context.Background(), listed)

	got := a.HealthReport(listed)
	if len(got) != 1 || got[0].Name != "blog" {
		t.Fatalf("report = %+v, want blog alone (elsewhere runs on another host)", got)
	}
	if got[0].State != statuspage.Down {
		t.Errorf("state = %s, want down after the probe failed", got[0].State)
	}
	if !slices.Equal(got[0].History.States, []statuspage.State{statuspage.Down}) {
		t.Errorf("history = %v, want one bucket holding the worst sample", got[0].History.States)
	}

	// Dropped from the list, dropped from memory.
	a.sampleHealth(context.Background(), nil)
	if got := a.HealthReport([]string{"blog"}); len(got) != 0 {
		t.Errorf("an unlisted service is still held: %+v", got)
	}
}

// The digest the CLI stamps on a config is what /v1/info reports, across a
// restart too, since doctor may ask long after the push.
func TestFleetConfigDigestIsReported(t *testing.T) {
	a := newAgent(t)
	if err := a.PutFleetConfig("# pilot fleet config digest: 0123456789abcdef\nnotifiers: {}\n"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + proto.PathInfo)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info proto.Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.ConfigDigest != "0123456789abcdef" {
		t.Errorf("info reports %q", info.ConfigDigest)
	}

	restarted, err := New(Options{Root: a.Layout.Root, Host: "web-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.FleetConfigDigest(); got != "0123456789abcdef" {
		t.Errorf("after a restart the digest is %q", got)
	}
}
