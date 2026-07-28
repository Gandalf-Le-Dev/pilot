package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

const staticSpec = `
name: blog
runtime: static
hosts: [web-1]
build: {command: "true", output: [dist/]}
expose:
  domains: [blog.example.com]
  static: {spa: true}
`

// newAgent builds an agent rooted in a temp directory, standing in for a host.
func newAgent(t *testing.T) *Agent {
	t.Helper()
	root := t.TempDir()
	a, err := New(Options{
		Root: root, Host: "web-1", Build: "test",
		Caddyfile: filepath.Join(root, "Caddyfile"),
		Snippets:  filepath.Join(root, "pilot.d"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// stageRelease lays out a release on disk the way the CLI would have.
func stageRelease(t *testing.T, a *Agent, service, id string, files map[string]string) {
	t.Helper()
	dir := a.Layout.Release(service, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPutServicePersistsAcrossRestart(t *testing.T) {
	a := newAgent(t)

	if _, err := a.PutService(staticSpec); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Service("blog"); !ok {
		t.Fatal("service not cached")
	}

	// A new agent over the same root must rediscover it — this is what lets
	// the daemon keep monitoring across a reboot with no CLI involved.
	restarted, err := New(Options{Root: a.Layout.Root, Host: "web-1"})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := restarted.Service("blog")
	if !ok {
		t.Fatal("cached definition not reloaded after restart")
	}
	if s.Runtime != "static" || s.Expose == nil {
		t.Errorf("definition lost detail: %+v", s)
	}
}

// One unparseable cache file must not take down monitoring for everything else
// on the box.
func TestBadCacheFileIsSkipped(t *testing.T) {
	a := newAgent(t)
	if _, err := a.PutService(staticSpec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.cacheDir(), "broken.yaml"), []byte("name: x\n  bad: [[["), 0o644); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(Options{Root: a.Layout.Root, Host: "web-1"})
	if err != nil {
		t.Fatalf("a bad cache file should not fail startup: %v", err)
	}
	if _, ok := restarted.Service("blog"); !ok {
		t.Error("the good service should still have loaded")
	}
}

func TestForgetService(t *testing.T) {
	a := newAgent(t)
	if _, err := a.PutService(staticSpec); err != nil {
		t.Fatal(err)
	}
	if err := a.ForgetService("blog"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Service("blog"); ok {
		t.Error("service still cached")
	}
	if _, err := os.Stat(filepath.Join(a.cacheDir(), "blog.yaml")); !os.IsNotExist(err) {
		t.Error("cache file not removed")
	}
}

// The deploy job is the core promise of phase 2: it runs in the daemon, so
// whoever asked for it can disappear without affecting the outcome.
func TestDeployJobRunsToCompletion(t *testing.T) {
	a := newAgent(t)
	stageRelease(t, a, "blog", "0001-aaaaaaa", map[string]string{"index.html": "<h1>v1</h1>"})

	job, err := a.StartDeploy(proto.DeployRequest{
		Service: "blog", Release: "0001-aaaaaaa", Spec: staticSpec, Verify: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != proto.JobPending && job.State != proto.JobRunning {
		t.Errorf("job should start immediately, got %q", job.State)
	}

	final := waitDone(t, a, job.ID)
	if final.State != proto.JobSucceeded {
		t.Fatalf("job failed: %s\n%s", final.Error, formatEvents(final))
	}

	// The symlink is the commit point; it must now name the new release.
	current, err := os.Readlink(a.Layout.Current("blog"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(current) != "0001-aaaaaaa" {
		t.Errorf("current = %q", current)
	}

	st, err := release.ReadState(a.Layout.Service("blog"), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "0001-aaaaaaa" || len(st.History) != 1 {
		t.Errorf("state = %+v", st)
	}
}

// A failing health check must return the previous release automatically —
// without any client being connected.
func TestFailedVerifyRollsBackAutomatically(t *testing.T) {
	a := newAgent(t)
	stageRelease(t, a, "blog", "0001-aaaaaaa", map[string]string{"index.html": "<h1>v1</h1>"})
	stageRelease(t, a, "blog", "0002-bbbbbbb", map[string]string{"index.html": "<h1>v2</h1>"})

	// First release goes live cleanly.
	first, err := a.StartDeploy(proto.DeployRequest{
		Service: "blog", Release: "0001-aaaaaaa", Spec: staticSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitDone(t, a, first.ID); got.State != proto.JobSucceeded {
		t.Fatalf("first deploy failed: %s", got.Error)
	}

	// Second release has a health check that cannot pass: nothing is listening
	// on that port.
	failing := staticSpec + "health: {tcp: {addr: \"127.0.0.1:1\"}, timeout: 2s, interval: 500ms}\n"
	second, err := a.StartDeploy(proto.DeployRequest{
		Service: "blog", Release: "0002-bbbbbbb", Spec: failing, Verify: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	final := waitDone(t, a, second.ID)
	if final.State != proto.JobFailed {
		t.Fatalf("job should have failed, got %q\n%s", final.State, formatEvents(final))
	}
	if !final.RolledBack {
		t.Errorf("job should report a rollback:\n%s", formatEvents(final))
	}

	current, err := os.Readlink(a.Layout.Current("blog"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(current) != "0001-aaaaaaa" {
		t.Errorf("current = %q, want the previous release restored", filepath.Base(current))
	}

	st, _ := release.ReadState(a.Layout.Service("blog"), "blog")
	if st.Current != "0001-aaaaaaa" {
		t.Errorf("state.Current = %q, want the rolled-back release", st.Current)
	}
	if last := st.LastDeploy(); last == nil || last.Outcome != release.OutcomeRolledBack {
		t.Errorf("history should record the rollback: %+v", st.History)
	}
}

// The very first release has nothing to roll back to; that must be reported
// plainly rather than pretended away.
func TestFailedFirstDeployHasNothingToRollBackTo(t *testing.T) {
	a := newAgent(t)
	stageRelease(t, a, "blog", "0001-aaaaaaa", map[string]string{"index.html": "x"})

	failing := staticSpec + "health: {tcp: {addr: \"127.0.0.1:1\"}, timeout: 1s, interval: 300ms}\n"
	job, err := a.StartDeploy(proto.DeployRequest{
		Service: "blog", Release: "0001-aaaaaaa", Spec: failing, Verify: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	final := waitDone(t, a, job.ID)
	if final.State != proto.JobFailed || final.RolledBack {
		t.Errorf("state=%q rolledBack=%v", final.State, final.RolledBack)
	}
	if !strings.Contains(formatEvents(final), "nothing to roll back to") {
		t.Errorf("events should say why:\n%s", formatEvents(final))
	}
}

// Two concurrent deploys of one service would race for the symlink.
func TestSecondDeployIsRefusedWhileOneIsInFlight(t *testing.T) {
	a := newAgent(t)
	stageRelease(t, a, "blog", "0001-aaaaaaa", map[string]string{"index.html": "x"})

	slow := staticSpec + "health: {tcp: {addr: \"127.0.0.1:1\"}, timeout: 5s, interval: 1s}\n"
	if _, err := a.StartDeploy(proto.DeployRequest{
		Service: "blog", Release: "0001-aaaaaaa", Spec: slow, Verify: true,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := a.StartDeploy(proto.DeployRequest{
		Service: "blog", Release: "0001-aaaaaaa", Spec: staticSpec,
	})
	if err == nil {
		t.Fatal("a second concurrent deploy should be refused")
	}
	if !strings.Contains(err.Error(), "in flight") {
		t.Errorf("error should explain: %v", err)
	}
}

func TestDeployRejectsBadInput(t *testing.T) {
	a := newAgent(t)

	tests := []struct {
		name string
		req  proto.DeployRequest
		want string
	}{
		{"malformed release", proto.DeployRequest{Service: "blog", Release: "nope", Spec: staticSpec}, "malformed release"},
		{"spec names another service", proto.DeployRequest{Service: "api", Release: "0001-aaaaaaa", Spec: staticSpec}, "request names"},
		{
			"observe-only service",
			proto.DeployRequest{Service: "db", Release: "0001-aaaaaaa",
				Spec: "name: db\nruntime: compose\nhosts: [web-1]\nmanage: observe\n"},
			"observe",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.StartDeploy(tc.req)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestRollbackJob(t *testing.T) {
	a := newAgent(t)
	stageRelease(t, a, "blog", "0001-aaaaaaa", map[string]string{"index.html": "v1"})
	stageRelease(t, a, "blog", "0002-bbbbbbb", map[string]string{"index.html": "v2"})

	for _, id := range []string{"0001-aaaaaaa", "0002-bbbbbbb"} {
		job, err := a.StartDeploy(proto.DeployRequest{Service: "blog", Release: id, Spec: staticSpec})
		if err != nil {
			t.Fatal(err)
		}
		if got := waitDone(t, a, job.ID); got.State != proto.JobSucceeded {
			t.Fatalf("deploy of %s failed: %s", id, got.Error)
		}
	}

	job, err := a.StartRollback(proto.RollbackRequest{Service: "blog"})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitDone(t, a, job.ID); got.State != proto.JobSucceeded {
		t.Fatalf("rollback failed: %s\n%s", got.Error, formatEvents(got))
	}

	current, _ := os.Readlink(a.Layout.Current("blog"))
	if filepath.Base(current) != "0001-aaaaaaa" {
		t.Errorf("current = %q, want the previous release", filepath.Base(current))
	}
}

func TestRollbackRefusesMissingRelease(t *testing.T) {
	a := newAgent(t)
	stageRelease(t, a, "blog", "0001-aaaaaaa", map[string]string{"index.html": "v1"})
	job, _ := a.StartDeploy(proto.DeployRequest{Service: "blog", Release: "0001-aaaaaaa", Spec: staticSpec})
	waitDone(t, a, job.ID)

	if _, err := a.StartRollback(proto.RollbackRequest{Service: "blog"}); err == nil {
		t.Error("with only one release there is nothing to roll back to")
	}
	if _, err := a.StartRollback(proto.RollbackRequest{Service: "blog", To: "0099-fffffff"}); err == nil {
		t.Error("rolling back to a release that is not on disk should be refused")
	}
}

// --- HTTP surface ---

func TestAPIInfoCarriesProtocolVersion(t *testing.T) {
	a := newAgent(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + proto.PathInfo)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get(proto.HeaderProtocol); got != strconv.Itoa(proto.Version) {
		t.Errorf("protocol header = %q, want %d", got, proto.Version)
	}

	var info proto.Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Protocol != proto.Version || info.Host != "web-1" {
		t.Errorf("info = %+v", info)
	}
}

func TestAPIStatusAndServiceLookup(t *testing.T) {
	a := newAgent(t)
	if _, err := a.PutService(staticSpec); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + proto.PathStatus)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var status proto.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if len(status.Services) != 1 || status.Services[0].Name != "blog" {
		t.Fatalf("status = %+v", status)
	}
	// Nothing is deployed yet, so it should say stopped rather than claim health.
	if status.Services[0].Obs.State != "stopped" {
		t.Errorf("state = %q, want stopped", status.Services[0].Obs.State)
	}

	resp2, err := http.Get(srv.URL + proto.PathServices + "nosuch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown service returned %s", resp2.Status)
	}
}

func TestAPIDeployAndFollowJob(t *testing.T) {
	a := newAgent(t)
	stageRelease(t, a, "blog", "0001-aaaaaaa", map[string]string{"index.html": "x"})

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	body, _ := json.Marshal(proto.DeployRequest{
		Service: "blog", Release: "0001-aaaaaaa", Spec: staticSpec,
	})
	resp, err := http.Post(srv.URL+proto.PathDeploy, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy returned %s", resp.Status)
	}

	var job proto.Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}

	// The long-poll should return once the job finishes.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(srv.URL + proto.PathJobs + job.ID + "?wait=1&after=0")
		if err != nil {
			t.Fatal(err)
		}
		var got proto.Job
		json.NewDecoder(r.Body).Decode(&got)
		r.Body.Close()
		if got.State.Done() {
			if got.State != proto.JobSucceeded {
				t.Fatalf("job failed: %s", got.Error)
			}
			return
		}
	}
	t.Fatal("job did not finish in time")
}

func TestJobStoreKeepsRunningJobsWhenTrimming(t *testing.T) {
	s := NewJobStore()

	running := s.Create("deploy", "busy", "0001-aaaaaaa", "")
	s.Start(running.ID)

	for i := range MaxJobs + 10 {
		j := s.Create("deploy", "svc"+strconv.Itoa(i), "0001-aaaaaaa", "")
		s.Finish(j.ID, nil, false)
	}

	if _, ok := s.Get(running.ID); !ok {
		t.Error("a job still in flight was trimmed away")
	}
	if got := len(s.List()); got > MaxJobs+1 {
		t.Errorf("store holds %d jobs, want trimming to have applied", got)
	}
}

func TestJobStoreWaitReturnsOnChange(t *testing.T) {
	s := NewJobStore()
	job := s.Create("deploy", "blog", "0001-aaaaaaa", "")

	done := make(chan *proto.Job, 1)
	go func() {
		got, _ := s.Wait(context.Background(), job.ID, 0)
		done <- got
	}()

	time.Sleep(20 * time.Millisecond)
	s.Event(job.ID, proto.PhaseActivate, "activating")

	select {
	case got := <-done:
		if len(got.Events) != 1 {
			t.Errorf("got %d events", len(got.Events))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after an event")
	}
}

func waitDone(t *testing.T, a *Agent, id string) *proto.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for {
		job, ok := a.Jobs().Wait(ctx, id, 0)
		if !ok {
			t.Fatalf("job %s disappeared", id)
		}
		if job.State.Done() {
			return job
		}
		if ctx.Err() != nil {
			t.Fatalf("job %s did not finish:\n%s", id, formatEvents(job))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func formatEvents(j *proto.Job) string {
	var b strings.Builder
	for _, e := range j.Events {
		b.WriteString("  [" + e.Phase + "] " + e.Message + "\n")
	}
	return b.String()
}
