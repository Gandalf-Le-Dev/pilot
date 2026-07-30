package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

type capture struct {
	mu   sync.Mutex
	sent []alert.Notification
}

func (c *capture) Send(_ context.Context, _ string, msg alert.Notification) error {
	c.mu.Lock()
	c.sent = append(c.sent, msg)
	c.mu.Unlock()
	return nil
}

func (c *capture) all() []alert.Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]alert.Notification(nil), c.sent...)
}

// notifyAgent returns an agent whose notifications are captured rather than sent.
func notifyAgent(t *testing.T, fleet *config.FleetConfig) (*Agent, *capture) {
	t.Helper()
	cap := &capture{}
	a := &Agent{
		Host:   "web-1",
		jobs:   NewJobStore(),
		alerts: alert.NewEngine("web-1", cap),
		fleet:  fleet,
	}
	a.jobs.OnFinish = a.notifyDeployFinished
	return a, cap
}

func withNotifier() *config.FleetConfig {
	return &config.FleetConfig{
		Notifiers: map[string]config.Notifier{"phone": {Type: "ntfy", URL: "https://ntfy.example/x"}},
	}
}

// TestFinishedDeployNotifiesWithItsUndo is the whole point: the user is not
// asked before a deploy, so they must be told after — and told how to reverse it.
func TestFinishedDeployNotifiesWithItsUndo(t *testing.T) {
	a, cap := notifyAgent(t, withNotifier())

	id := a.jobs.Create(proto.KindDeploy, "wakapi", "0042-9f3ac1b", "").ID
	a.jobs.Finish(id, nil, false)
	a.alerts.Flush()

	sent := cap.all()
	if len(sent) != 1 {
		t.Fatalf("got %d notifications, want 1", len(sent))
	}
	msg := sent[0]

	if msg.Severity != alert.SevEvent {
		t.Errorf("severity = %q, want an event rather than an alert", msg.Severity)
	}
	if strings.HasPrefix(msg.Title(), "ALERT") {
		t.Errorf("a routine deploy must not read as an alarm: %q", msg.Title())
	}
	if !strings.Contains(msg.Summary, "wakapi") || !strings.Contains(msg.Summary, "0042-9f3ac1b") {
		t.Errorf("summary should name the service and release: %q", msg.Summary)
	}
	if !strings.Contains(msg.Text(), "pilot rollback wakapi") {
		t.Errorf("the undo command must travel with the notification:\n%s", msg.Text())
	}
}

func TestFailedDeployReportsWhetherItRolledBack(t *testing.T) {
	a, cap := notifyAgent(t, withNotifier())

	id := a.jobs.Create(proto.KindDeploy, "kite", "0007-abc1234", "").ID
	a.jobs.Finish(id, errFake("health check failed"), true)
	a.alerts.Flush()

	msg := cap.all()[0]
	if !strings.Contains(msg.Summary, "rolled back") {
		t.Errorf("a rolled-back failure must say so: %q", msg.Summary)
	}
	// Offering `pilot rollback` after an automatic rollback would tell someone
	// to undo the recovery.
	if strings.Contains(msg.Text(), "pilot rollback") {
		t.Errorf("must not offer to roll back what already rolled itself back:\n%s", msg.Text())
	}
}

func TestRollbackIsNotOfferedItsOwnUndo(t *testing.T) {
	a, cap := notifyAgent(t, withNotifier())

	id := a.jobs.Create(proto.KindRollback, "docmost", "0001-8604364", "0002-deadbee").ID
	a.jobs.Finish(id, nil, false)
	a.alerts.Flush()

	msg := cap.all()[0]
	if !strings.Contains(msg.Summary, "rolled back") {
		t.Errorf("summary = %q", msg.Summary)
	}
	if strings.Contains(msg.Text(), "pilot rollback docmost") {
		t.Errorf("rolling back a rollback moves forward again; do not suggest it:\n%s", msg.Text())
	}
}

// TestNotifyDeploysCanBeTurnedOff — the setting exists because someone will find
// their own deploys noisy, and the alternative to a switch is them muting the
// notifier entirely and losing real alerts with it.
func TestNotifyDeploysCanBeTurnedOff(t *testing.T) {
	off := false
	fleet := withNotifier()
	fleet.NotifyDeploys = &off

	a, cap := notifyAgent(t, fleet)
	id := a.jobs.Create(proto.KindDeploy, "wakapi", "0042-9f3ac1b", "").ID
	a.jobs.Finish(id, nil, false)
	a.alerts.Flush()

	if n := len(cap.all()); n != 0 {
		t.Errorf("got %d notifications with notify_deploys: false", n)
	}
}

func TestDefaultIsOn(t *testing.T) {
	fleet := withNotifier() // NotifyDeploys left nil
	if !fleet.DeployNotificationsEnabled() {
		t.Fatal("unset must mean enabled; a deploy nobody hears about is the case this exists for")
	}
}

// TestNoNotifiersIsSilent — nothing configured means nowhere to send, and
// attempting delivery would only produce errors in the journal.
func TestNoNotifiersIsSilent(t *testing.T) {
	a, cap := notifyAgent(t, &config.FleetConfig{})
	id := a.jobs.Create(proto.KindDeploy, "wakapi", "0042-9f3ac1b", "").ID
	a.jobs.Finish(id, nil, false)
	a.alerts.Flush()

	if n := len(cap.all()); n != 0 {
		t.Errorf("got %d notifications with no notifiers configured", n)
	}
}

// TestNoFleetConfigDoesNotPanic covers a host bootstrapped but never pushed a
// config — a deploy there must still complete.
func TestNoFleetConfigDoesNotPanic(t *testing.T) {
	a, _ := notifyAgent(t, nil)
	id := a.jobs.Create(proto.KindDeploy, "wakapi", "0042-9f3ac1b", "").ID
	a.jobs.Finish(id, nil, false)
	a.alerts.Flush()
}

type errFake string

func (e errFake) Error() string { return string(e) }

// TestFleetConfigIsNotLoadedAsAService is a regression test for a misleading log
// line found by reading a real agent's journal.
//
// The service cache and the host-wide config share a directory, and the loader
// took every *.yaml in it as a service definition — so `_fleet.yaml` failed to
// parse and the agent announced `unknown field "notifiers"` at every start. The
// config was in fact loaded correctly, a few lines later, by its own loader.
// Nothing was broken except the operator's confidence.
func TestFleetConfigIsNotLoadedAsAService(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{
		Layout:   release.NewLayout(dir),
		services: map[string]*config.Service{},
		drift:    map[string]*driftRecord{},
		jobs:     NewJobStore(),
		alerts:   alert.NewEngine("web-1", nil),
	}

	if err := os.MkdirAll(a.cacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(a.cacheDir(), name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(FleetConfigFile, "notifiers:\n  phone:\n    type: ntfy\n    url: https://x\nalerts: []\n")
	write("wakapi.yaml", "name: wakapi\nruntime: compose\nhosts: [web-1]\n")

	if err := a.loadCache(); err != nil {
		t.Fatal(err)
	}

	if _, ok := a.services["wakapi"]; !ok {
		t.Error("the real service definition was not loaded")
	}
	// The symptom was a log line, but the cause is observable here: the loader
	// used to reach the fleet config at all. It no longer opens it, so it cannot
	// complain about it.
	for name := range a.services {
		if strings.HasPrefix(name, "_") || name == "" {
			t.Errorf("loaded %q as a service; the host-wide config is not one", name)
		}
	}
}
