// Package registry maps a configured runtime to its adapter.
//
// It exists because there were two of these switches — one in internal/app for
// the CLI, one in internal/agent for the daemon — and they were allowed to
// disagree. Implementing the systemd runtime meant updating both, only one got
// updated, and the result was a deploy that planned cleanly on the operator's
// machine and then failed on the host with "the systemd runtime is not
// implemented in this build". The CLI said supported; the agent said no; nothing
// compared them.
//
// One switch cannot disagree with itself. The test alongside this file asserts
// that every value in config.AllRuntimes resolves, so adding a runtime constant
// without an adapter fails the build rather than a deploy.
package registry

import (
	"fmt"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime/compose"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime/static"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime/systemd"
)

// For selects the adapter for a service.
//
// This switch is the only place that knows the full set of runtimes; adding one
// means implementing the interface and adding a case here.
func For(s *config.Service) (runtime.Runtime, error) {
	switch s.Runtime {
	case config.RuntimeCompose:
		return compose.New(), nil
	case config.RuntimeStatic:
		return static.New(), nil
	case config.RuntimeSystemd:
		return systemd.New(), nil
	}
	return nil, fmt.Errorf("service %q has unknown runtime %q", s.Name, s.Runtime)
}
