// Package runtime defines the adapter every kind of workload implements, plus
// the machinery all of them share: staging a release directory, swapping the
// `current` symlink, and reporting what is actually running.
//
// The interface is deliberately small. Adding a new kind of workload — a k3s
// adapter, a Caddy-config-only service — means implementing six methods and
// changing nothing else.
package runtime

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// Runtime adapts one kind of workload to Pilot's deploy model.
type Runtime interface {
	// Kind identifies the runtime this adapter implements.
	Kind() config.Runtime

	// Stage prepares a release on the host without affecting what is running.
	// Everything that can fail should fail here — image pulls, config
	// rendering, validation — so that Activate is as close to instant and
	// as unlikely to fail as we can make it.
	Stage(ctx context.Context, t *Target, in StageInput) error

	// Activate makes the staged release live. This is the commit point.
	Activate(ctx context.Context, t *Target) error

	// Deactivate stops the service without destroying its releases.
	Deactivate(ctx context.Context, t *Target) error

	// Observe reports what is actually running right now.
	Observe(ctx context.Context, t *Target) (Observation, error)

	// Logs streams service logs to w.
	Logs(ctx context.Context, t *Target, opts LogOptions, w io.Writer) error

	// Fingerprint hashes the live configuration, for drift detection against
	// the release manifest.
	Fingerprint(ctx context.Context, t *Target) (string, error)
}

// Target is one (service, host) pair plus the means to act on it.
//
// Client is an interface rather than an ssh client on purpose: the agent runs
// these same adapters in-process against the local filesystem, and a runtime
// should never have needed to know whether its commands crossed a network.
type Target struct {
	Service *config.Service
	Host    *config.Host
	Layout  release.Layout
	Client  transport.Executor

	// Release is the release ID being staged or activated. Empty for
	// operations that act on whatever is currently live.
	Release string
}

// ServiceDir is the target's per-service directory on the host.
func (t *Target) ServiceDir() string { return t.Layout.Service(t.Service.Name) }

// ReleaseDir is the directory for t.Release.
func (t *Target) ReleaseDir() string { return t.Layout.Release(t.Service.Name, t.Release) }

// CurrentDir is the path services should reference, so that a symlink swap is
// all it takes to change what runs.
func (t *Target) CurrentDir() string { return t.Layout.Current(t.Service.Name) }

// Label identifies the target in output, e.g. "api on web-1".
func (t *Target) Label() string {
	return fmt.Sprintf("%s on %s", t.Service.Name, t.Host.Name)
}

// StageInput is everything the CLI has prepared locally for one release.
type StageInput struct {
	// LocalDir holds the release contents assembled on the operator's machine:
	// rendered compose file, built static assets, binaries.
	LocalDir string

	// Env is the resolved environment. It is written separately from LocalDir
	// so it can be given 0600 and kept out of any archive or log.
	Env map[string]string

	// Manifest describes the release. It records env *names* and a digest,
	// never values.
	Manifest *release.Manifest

	// Snippet is the rendered Caddy route, empty when the service is not
	// exposed. A copy lives in the release so a rollback restores routing and
	// code together.
	Snippet string
}

// State is a service's coarse condition, uniform across runtimes.
type State string

const (
	StateRunning  State = "running"
	StateDegraded State = "degraded"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
	StateUnknown  State = "unknown"
)

// Observation is what a runtime reports about a live service.
type Observation struct {
	State     State      `json:"state"`
	Release   string     `json:"release,omitempty"`
	Since     time.Time  `json:"since,omitzero"`
	Instances []Instance `json:"instances,omitempty"`

	// Detail explains a non-running state in one line.
	Detail string `json:"detail,omitempty"`
}

// Instance is one container or process backing a service.
type Instance struct {
	Name     string    `json:"name"`
	State    string    `json:"state"`
	Health   string    `json:"health,omitempty"`
	Image    string    `json:"image,omitempty"`
	Restarts int       `json:"restarts,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	Detail   string    `json:"detail,omitempty"`
}

// LogOptions selects which logs to stream.
type LogOptions struct {
	Follow bool
	Tail   int
	Since  string
}

// ErrNoLogs is returned by runtimes that have no per-service log stream.
var ErrNoLogs = fmt.Errorf("this runtime has no service logs")
