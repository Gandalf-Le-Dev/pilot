package registry

import (
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

// TestEveryRuntimeResolves is the guard the duplicated switches never had.
//
// config.AllRuntimes is the list the validator accepts, so anything in it can
// reach a deploy. If a constant is added there without an adapter here, the
// failure used to arrive on the host, mid-deploy, after staging — and for the
// systemd runtime it arrived only on the agent, because the CLI's copy of this
// switch had already been updated and the agent's had not.
func TestEveryRuntimeResolves(t *testing.T) {
	for _, kind := range config.AllRuntimes {
		t.Run(string(kind), func(t *testing.T) {
			rt, err := For(&config.Service{Name: "x", Runtime: kind})
			if err != nil {
				t.Fatalf("%s does not resolve to an adapter: %v", kind, err)
			}
			if rt.Kind() != kind {
				t.Errorf("%s resolved to an adapter reporting Kind() = %q", kind, rt.Kind())
			}
		})
	}
}

func TestUnknownRuntimeIsAnError(t *testing.T) {
	if _, err := For(&config.Service{Name: "x", Runtime: "podman"}); err == nil {
		t.Error("an unknown runtime should not resolve")
	}
}
