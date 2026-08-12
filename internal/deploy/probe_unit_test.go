package deploy

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
)

// stubRuntime returns a fixed observation.
type stubRuntime struct{ obs runtime.Observation }

func (s stubRuntime) Kind() config.Runtime { return config.RuntimeSystemd }
func (s stubRuntime) Observe(context.Context, *runtime.Target) (runtime.Observation, error) {
	return s.obs, nil
}
func (s stubRuntime) Stage(context.Context, *runtime.Target, runtime.StageInput) error { return nil }
func (s stubRuntime) Activate(context.Context, *runtime.Target) error                  { return nil }
func (s stubRuntime) Deactivate(context.Context, *runtime.Target) error                { return nil }
func (s stubRuntime) Logs(context.Context, *runtime.Target, runtime.LogOptions, io.Writer) error {
	return nil
}
func (s stubRuntime) Fingerprint(context.Context, *runtime.Target) (string, error) { return "", nil }

// The bug this exists to prevent, seen on a live host: a backup whose last run
// predated its freshness bound could not be repaired by deploying. The gate
// failed on a condition the deploy had no power over, so every fix pushed to a
// stale service was rolled straight back out — including the fix for whatever
// had stopped it running.
func TestProbeUnitIgnoresStateAwaitingARun(t *testing.T) {
	for _, obs := range []runtime.Observation{
		{
			State:       runtime.StateDegraded,
			Detail:      "last succeeded 60d ago, past the 48h freshness bound",
			AwaitingRun: true,
		},
		{
			State:       runtime.StateStopped,
			Detail:      "backup.service has never run",
			AwaitingRun: true,
		},
	} {
		if err := probeUnit(context.Background(), stubRuntime{obs}, nil); err != nil {
			t.Errorf("gate rejected a state no deploy can change (%s): %v", obs.Detail, err)
		}
	}
}

// The flag must not become a way for real failures to pass. A unit that failed
// its last run is a verdict the deploy can and should be judged against.
func TestProbeUnitStillCatchesRealFailures(t *testing.T) {
	for _, obs := range []runtime.Observation{
		{State: runtime.StateFailed, Detail: "last run failed (result=exit-code, exit=1)"},
		{State: runtime.StateStopped, Detail: "unit is inactive"},
		{State: runtime.StateDegraded, Detail: "activating"},
	} {
		err := probeUnit(context.Background(), stubRuntime{obs}, nil)
		if err == nil {
			t.Errorf("gate accepted a genuinely unhealthy unit: %+v", obs)
			continue
		}
		if !strings.Contains(err.Error(), obs.Detail) {
			t.Errorf("error %q should carry the runtime's explanation %q", err, obs.Detail)
		}
	}
}

func TestProbeUnitAcceptsRunning(t *testing.T) {
	if err := probeUnit(context.Background(), stubRuntime{runtime.Observation{State: runtime.StateRunning}}, nil); err != nil {
		t.Errorf("a running unit should pass: %v", err)
	}
}
