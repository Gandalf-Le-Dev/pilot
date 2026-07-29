package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/ssh"
)

// fakeAgents answers for one host.
type fakeAgents struct {
	rep      AgentReport
	upgraded []string
	err      error
}

func (f *fakeAgents) Status(context.Context, string) AgentReport { return f.rep }

func (f *fakeAgents) Upgrade(_ context.Context, host string) error {
	f.upgraded = append(f.upgraded, host)
	return f.err
}

func agentEnv(a Agents) *Env {
	return &Env{
		Fleet:   &config.Fleet{Hosts: map[string]*config.Host{"web": {Name: "web"}}},
		Clients: map[string]*ssh.Client{"web": {}},
		Agents:  a,
	}
}

// TestSkewedAgentIsReportedAndFixable is the point of the whole check.
//
// A skewed agent used to surface only as a failure part-way through some later
// command, and the consequence was quiet: a deploy to that host proceeded
// without the daemon, dropping the automatic rollback.
func TestSkewedAgentIsReportedAndFixable(t *testing.T) {
	fake := &fakeAgents{rep: AgentReport{Installed: true, Skewed: true, Protocol: 2, Expected: 3}}

	found := checkAgents(context.Background(), agentEnv(fake))
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1", len(found))
	}
	f := found[0]

	if f.Status != StatusWarn {
		t.Errorf("status = %v, want warn", f.Status)
	}
	if !f.Fixable() {
		t.Fatal("a skewed agent is repairable in place; the finding must carry a fix")
	}
	if !strings.Contains(f.Hint, "pilot agent upgrade") {
		t.Errorf("hint should name the command that fixes it, got %q", f.Hint)
	}
	// The consequence has to be stated. "Protocol mismatch" alone does not tell
	// anyone they are about to lose their safety net.
	if !strings.Contains(f.Detail, "rollback") {
		t.Errorf("detail should say what the mismatch costs, got %q", f.Detail)
	}

	if err := f.Fix(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.upgraded) != 1 || fake.upgraded[0] != "web" {
		t.Errorf("fix upgraded %v, want [web]", fake.upgraded)
	}
}

// TestMissingAgentIsNotAutoFixed keeps a deliberate line: replacing a running
// agent is a repair, installing one where there was never any is setup, and
// `doctor --fix` should not quietly do setup.
func TestMissingAgentIsNotAutoFixed(t *testing.T) {
	fake := &fakeAgents{rep: AgentReport{Installed: false}}

	found := checkAgents(context.Background(), agentEnv(fake))
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1", len(found))
	}
	if found[0].Fixable() {
		t.Error("installing a first agent must stay an explicit command")
	}
	if !strings.Contains(found[0].Hint, "bootstrap") {
		t.Errorf("hint should point at bootstrap, got %q", found[0].Hint)
	}
	if len(fake.upgraded) != 0 {
		t.Errorf("nothing should have been installed, got %v", fake.upgraded)
	}
}

func TestCurrentAgentReportsOK(t *testing.T) {
	fake := &fakeAgents{rep: AgentReport{Installed: true, Build: "0.2.0", Protocol: 3, Expected: 3}}

	found := checkAgents(context.Background(), agentEnv(fake))
	if len(found) != 1 || found[0].Status != StatusOK {
		t.Fatalf("got %+v, want a single OK finding", found)
	}
	if !strings.Contains(found[0].Title, "0.2.0") {
		t.Errorf("title should name the build, got %q", found[0].Title)
	}
}

// TestUnreachableAgentIsNotDoubleReported avoids a cascade: reachability
// already reported the host once, and repeating it per-check turns one problem
// into a wall of noise.
func TestUnreachableAgentIsNotDoubleReported(t *testing.T) {
	fake := &fakeAgents{rep: AgentReport{Unreachable: true}}
	if found := checkAgents(context.Background(), agentEnv(fake)); len(found) != 0 {
		t.Errorf("got %d findings, want none", len(found))
	}

	// Same when the host never connected at all.
	env := agentEnv(fake)
	env.Clients = map[string]*ssh.Client{}
	if found := checkAgents(context.Background(), env); len(found) != 0 {
		t.Errorf("got %d findings for an unreachable host, want none", len(found))
	}
}

// TestNoAgentsHookSkipsCheck keeps `--offline` and the unit tests free of it.
func TestNoAgentsHookSkipsCheck(t *testing.T) {
	env := agentEnv(nil)
	env.Agents = nil
	if found := checkAgents(context.Background(), env); found != nil {
		t.Errorf("got %v, want nil when no hook is wired", found)
	}
}

// TestFixFailurePropagates — a fix that reports success it did not achieve is
// worse than one that fails loudly.
func TestFixFailurePropagates(t *testing.T) {
	fake := &fakeAgents{
		rep: AgentReport{Installed: true, Skewed: true, Protocol: 2, Expected: 3},
		err: errors.New("download failed"),
	}
	found := checkAgents(context.Background(), agentEnv(fake))
	if err := found[0].Fix(context.Background()); err == nil {
		t.Fatal("want the underlying error")
	}
}
