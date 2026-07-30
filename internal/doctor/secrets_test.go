package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/ssh"
)

type fakeSampler struct {
	sample *LogSample
	err    error
	asked  int
}

func (f *fakeSampler) Sample(context.Context, string, string, int) (*LogSample, error) {
	f.asked++
	return f.sample, f.err
}

func logEnv(s LogSampler) *Env {
	return &Env{
		Fleet: &config.Fleet{
			Hosts:    map[string]*config.Host{"web": {Name: "web"}},
			Services: map[string]*config.Service{"wakapi": {Name: "wakapi", Hosts: []string{"web"}}},
		},
		Clients: map[string]*ssh.Client{"web": {}},
		Logs:    s,
	}
}

// TestReportsCredentialsWithoutQuotingThem is the property that matters: the
// finding travels to a terminal, a JSON document, and possibly a CI log, so it
// must name what leaked and never repeat it.
func TestReportsCredentialsWithoutQuotingThem(t *testing.T) {
	secret := "26dba175-be35-4919-886a-4ab0509c1a07"
	f := &fakeSampler{sample: &LogSample{
		Text: `INFO uri="/api/heartbeats.bulk?api_key=` + secret + `" status=201`,
	}}

	found := checkLogSecrets(context.Background(), logEnv(f))
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1", len(found))
	}
	got := found[0]

	if got.Status != StatusWarn {
		t.Errorf("status = %v, want warn", got.Status)
	}
	if !strings.Contains(got.Title, "wakapi") || !strings.Contains(got.Title, "api_key") {
		t.Errorf("title should name the service and what leaked, got %q", got.Title)
	}
	for _, field := range []string{got.Title, got.Detail, got.Hint} {
		if strings.Contains(field, secret) {
			t.Fatalf("the finding repeats the credential it is reporting: %q", field)
		}
	}

	// Nothing Pilot can do to a host stops a service writing a secret to
	// stdout, so offering to "fix" it would be a lie.
	if got.Fixable() {
		t.Error("a logged credential is not repairable by Pilot; the application must change")
	}
}

func TestCleanLogsProduceNoFinding(t *testing.T) {
	f := &fakeSampler{sample: &LogSample{
		Text: "INFO activated release 0042-9f3ac1b\nINFO GET /health 200 1.2ms",
	}}
	if found := checkLogSecrets(context.Background(), logEnv(f)); len(found) != 0 {
		t.Errorf("got %+v, want no findings", found)
	}
}

// TestUnreadableLogsAreNotAFinding — a service that has not started has no
// logs, and other checks already report that. Saying it again here would turn
// one problem into two.
func TestUnreadableLogsAreNotAFinding(t *testing.T) {
	f := &fakeSampler{err: errors.New("no such container")}
	if found := checkLogSecrets(context.Background(), logEnv(f)); len(found) != 0 {
		t.Errorf("got %+v, want no findings", found)
	}
}

func TestNoSamplerSkipsTheCheck(t *testing.T) {
	env := logEnv(nil)
	env.Logs = nil
	if found := checkLogSecrets(context.Background(), env); found != nil {
		t.Errorf("got %+v, want nil when no sampler is wired", found)
	}
}

// TestUnreachableHostIsNotSampled avoids both a cascade of findings and a
// pointless round trip to a host already known to be down.
func TestUnreachableHostIsNotSampled(t *testing.T) {
	f := &fakeSampler{sample: &LogSample{Text: "api_key=aaaaaaaa"}}
	env := logEnv(f)
	env.Clients = map[string]*ssh.Client{}

	if found := checkLogSecrets(context.Background(), env); len(found) != 0 {
		t.Errorf("got %+v, want no findings", found)
	}
	if f.asked != 0 {
		t.Errorf("sampled an unreachable host %d time(s)", f.asked)
	}
}
