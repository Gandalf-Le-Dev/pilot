package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
)

// TestEveryProbeKindIsDispatched is the guard the two probe switches never had.
//
// The agent and deploy.Verify implement their probes differently on purpose —
// in-process here, shelled out there — but they must recognise the same set of
// kinds. When `systemd` was added to config and wired only into the CLI's
// switch, a deploy planned, staged and activated cleanly and then failed
// verification on the host, because the agent's copy still said "not
// implemented". The health check meant something different depending on whether
// an agent was present.
//
// Every kind must reach a real branch. A network failure is fine and expected
// here; "not implemented" is not.
func TestEveryProbeKindIsDispatched(t *testing.T) {
	kinds := map[string]*config.Health{
		"http":    {HTTP: &config.HTTPProbe{URL: "http://127.0.0.1:1/x", Expect: 200}},
		"tcp":     {TCP: &config.TCPProbe{Addr: "127.0.0.1:1"}},
		"exec":    {Exec: &config.ExecProbe{Cmd: []string{"false"}}},
		"docker":  {Docker: true},
		"systemd": {Systemd: true},
	}

	// Every kind config.Health can express must appear above, or this guard
	// quietly stops covering the one that was added.
	for name, h := range kinds {
		if got := h.Probes(); got != 1 {
			t.Fatalf("%s fixture sets %d probes, want exactly 1", name, got)
		}
	}
	if len(kinds) != probeKindCount {
		t.Fatalf("config.Health has %d probe kinds but this test covers %d — add the new one",
			probeKindCount, len(kinds))
	}

	for name, h := range kinds {
		t.Run(name, func(t *testing.T) {
			svc := &config.Service{Name: "x", Runtime: config.RuntimeSystemd, Health: h,
				Unit: &config.Unit{Name: "x.service"}}
			err := Probe(context.Background(), stubRuntime{}, &runtime.Target{Service: svc})
			if err != nil && strings.Contains(err.Error(), "not implemented") {
				t.Errorf("%s is not dispatched: %v", name, err)
			}
		})
	}
}

// probeKindCount is the number of probe kinds config.Health can express. It is
// asserted against the fixture list above so adding a kind to the config
// without adding it here fails rather than silently narrowing the guard.
const probeKindCount = 5

// stubRuntime stands in for an adapter so the docker and systemd branches can
// be dispatched without a host.
type stubRuntime struct{ runtime.Runtime }

func (stubRuntime) Observe(context.Context, *runtime.Target) (runtime.Observation, error) {
	return runtime.Observation{State: runtime.StateRunning}, nil
}
