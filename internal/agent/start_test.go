package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// deployTwo puts 0002 live with 0001 as its rollback target.
func deployTwo(t *testing.T, a *Agent) {
	t.Helper()
	for _, id := range []string{"0001-aaaaaaa", "0002-bbbbbbb"} {
		stageRelease(t, a, "blog", id, map[string]string{"index.html": id})
		job, err := a.StartDeploy(proto.DeployRequest{Service: "blog", Release: id, Spec: staticSpec})
		if err != nil {
			t.Fatal(err)
		}
		if got := waitDone(t, a, job.ID); got.State != proto.JobSucceeded {
			t.Fatalf("deploying %s failed: %s", id, got.Error)
		}
	}
}

// Seen on a VPS after a reboot: the live release was fine, but its port bound
// to an address that did not exist yet. Rolling back to older code would not
// fix that and would undo a deploy someone chose, so a failed start is
// reported and left alone.
func TestStartingTheLiveReleaseNeverRollsBack(t *testing.T) {
	a := newAgent(t)
	deployTwo(t, a)

	failing := staticSpec + "health: {tcp: {addr: \"127.0.0.1:1\"}, timeout: 1s, interval: 300ms}\n"
	job, err := a.StartDeploy(proto.DeployRequest{
		Service: "blog", Release: "0002-bbbbbbb", Spec: failing, Verify: true, Start: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	final := waitDone(t, a, job.ID)
	if final.State != proto.JobFailed || final.RolledBack {
		t.Fatalf("state=%q rolledBack=%v\n%s", final.State, final.RolledBack, formatEvents(final))
	}
	if !strings.Contains(formatEvents(final), "not rolling back") {
		t.Errorf("events should say why nothing was rolled back:\n%s", formatEvents(final))
	}

	current, err := os.Readlink(a.Layout.Current("blog"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(current) != "0002-bbbbbbb" {
		t.Errorf("current = %q, want the live release left in place", filepath.Base(current))
	}

	st, _ := release.ReadState(a.Layout.Service("blog"), "blog")
	if st.Current != "0002-bbbbbbb" || st.Previous != "0001-aaaaaaa" {
		t.Errorf("state = current %q previous %q, want both unchanged", st.Current, st.Previous)
	}
	if last := st.LastDeploy(); last == nil || last.Outcome != release.OutcomeFailed {
		t.Errorf("history should record the failed start: %+v", st.History)
	}
}

// Promoting the live release onto itself must not shift it into Previous, and
// pruning must not count the real rollback target as superseded.
func TestStartingTheLiveReleaseKeepsTheRollbackTarget(t *testing.T) {
	a := newAgent(t)
	deployTwo(t, a)

	job, err := a.StartDeploy(proto.DeployRequest{
		Service: "blog", Release: "0002-bbbbbbb", Spec: staticSpec, KeepReleases: 1, Start: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if final := waitDone(t, a, job.ID); final.State != proto.JobSucceeded {
		t.Fatalf("start failed: %s\n%s", final.Error, formatEvents(final))
	}
	if job.Kind != proto.KindStart {
		t.Errorf("job kind = %q, want %q so that notifications call it a start", job.Kind, proto.KindStart)
	}

	st, _ := release.ReadState(a.Layout.Service("blog"), "blog")
	if st.Current != "0002-bbbbbbb" || st.Previous != "0001-aaaaaaa" {
		t.Errorf("state = current %q previous %q, want both unchanged", st.Current, st.Previous)
	}
	last := st.LastDeploy()
	if last == nil || last.Outcome != release.OutcomeOK || last.Reason != release.ReasonStarted {
		t.Errorf("history should record a start, not a deploy: %+v", last)
	}
	if ok, _ := a.Exec.Exists(t.Context(), a.Layout.Release("blog", "0001-aaaaaaa")); !ok {
		t.Error("the rollback target was pruned")
	}
}

// A start and a deploy are never inferred from each other: a plan that went
// stale while it was on screen must be refused, not reinterpreted.
func TestStartAndDeployAreNotConfused(t *testing.T) {
	tests := []struct {
		name    string
		release string
		start   bool
		want    string
	}{
		{"start of a release that is not live", "0001-aaaaaaa", true, "asked to start"},
		{"deploy of the live release", "0002-bbbbbbb", false, "already runs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAgent(t)
			deployTwo(t, a)

			_, err := a.StartDeploy(proto.DeployRequest{
				Service: "blog", Release: tc.release, Spec: staticSpec, Start: tc.start,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			st, _ := release.ReadState(a.Layout.Service("blog"), "blog")
			if st.Current != "0002-bbbbbbb" || len(st.History) != 2 {
				t.Errorf("a refused request must change nothing: %+v", st)
			}
		})
	}
}
