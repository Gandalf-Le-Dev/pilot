// Package proto is the wire contract between the CLI and the agent.
//
// The agent serves HTTP over a Unix socket. HTTP rather than a bespoke
// stdio protocol because it costs nothing here and buys a great deal later:
// the phase 4 dashboard speaks to the same handlers over mTLS, and every
// endpoint is testable with net/http/httptest.
package proto

import (
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
)

// Version is the protocol version. The CLI refuses to talk to an agent that
// does not match, and says to re-bootstrap, rather than failing in some
// creative way halfway through a deploy.
//
// Bump this for a change to the wire format *or to the configuration schema*.
// Both travel between the CLI and the agent, so both can be misunderstood, and
// only one of them used to be covered: `expose.allow` was added to the config
// while this stayed at 2, the handshake therefore passed, and the agent then
// rejected the service definition with `unknown field "allow"` — after the
// deploy had already begun. SchemaDigest below now makes that omission a
// failing test rather than a failed deploy.
//
// Version 4 added `notify_deploys` to the host-wide config, and closed a second
// hole while doing it: the digest covered `Fleet` and `Service` but not the
// host-wide config the agent also parses strictly, so a field added there would
// have gone through unguarded — the same gap in a different struct.
//
// Version 5 added PUT /v1/services/{name}, so a no-op deploy can still refresh
// the spec an agent observes, probes and alerts with.
//
// Version 6 reshaped the systemd `unit` block. It used to describe a unit for
// Pilot to write (exec_start, user, restart); it now names one to adopt, plus
// the links, precheck and freshness bound the runtime needs. Every old field is
// gone rather than deprecated — the runtime was never implemented, so no
// working configuration could reference them.
//
// Version 7 added `unit.reference`, naming the recorded definition a unit is
// expected to match. Pilot never installs units, so without this the only
// record of one is the host itself, and Fingerprint cannot help: it compares a
// unit against whatever a deploy last saw, so a unit wrong from the first
// deploy is drift from nothing. The field is operator-side, but it travels in
// the service spec, so an older agent would reject it.
//
// Version 8 added `caddy_bind` to the host-wide config, mirroring the new
// hosts.<name>.caddy.bind: on a host whose Caddyfile binds sites to explicit
// addresses, generated routes must bind the same way or they land in a server
// public traffic never reaches. The agent needs the value because a
// blue/green flip re-renders the route itself.
//
// Version 9 added GET /v1/dashboard — status, resource samples, alert
// episodes, and deploy history in one response, because each call from the
// operator's machine costs an ssh exec. The config schema is unchanged; the
// bump is for the wire.
const Version = 9

// SchemaDigest pins the configuration schema this protocol version speaks.
//
// It must equal config.SchemaFingerprint(); a test enforces that. When the
// test fails you have changed the schema, and the fix is one of:
//
//   - The change adds or renames a field the agent must understand: bump
//     Version above, then update this digest. Older agents will be told to
//     re-bootstrap instead of failing mid-deploy.
//   - The change is invisible to the agent (a comment, a field it never
//     decodes): update this digest alone.
//
// The choice is deliberately yours. What is not optional is making it.
const SchemaDigest = "9473ec986c56dc1a"

const (
	// DefaultSocket is where the daemon listens. A Unix socket rather than a
	// port means no new inbound attack surface: reaching it requires already
	// being on the box.
	DefaultSocket = "/run/pilot.sock"

	// HeaderProtocol carries the agent's protocol version on every response.
	HeaderProtocol = "X-Pilot-Protocol"
)

// Info identifies an agent.
type Info struct {
	Protocol  int       `json:"protocol"`
	Build     string    `json:"build"`
	Host      string    `json:"host"`
	StartedAt time.Time `json:"started_at"`
	Services  int       `json:"services"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}

// ServiceStatus is what the agent knows about one service.
type ServiceStatus struct {
	Name    string              `json:"name"`
	Runtime string              `json:"runtime"`
	Manage  string              `json:"manage"`
	Obs     runtime.Observation `json:"observation"`

	// Drift is set when the live configuration no longer matches the manifest
	// of the release that is supposed to be running.
	Drift *Drift `json:"drift,omitempty"`

	// Error explains why this service could not be observed at all.
	Error string `json:"error,omitempty"`
}

// StatusResponse is the agent's view of everything it manages.
type StatusResponse struct {
	Host     string          `json:"host"`
	Services []ServiceStatus `json:"services"`
	Disk     *DiskUsage      `json:"disk,omitempty"`
}

// DiskUsage reports headroom on the volume holding Pilot's releases.
type DiskUsage struct {
	Path        string `json:"path"`
	UsedPercent int    `json:"used_percent"`
}

// Drift is a divergence between the live configuration and the manifest.
type Drift struct {
	DetectedAt time.Time `json:"detected_at"`
	Release    string    `json:"release"`

	// Config is set when the runtime's fingerprint no longer matches what was
	// recorded at deploy time — a hand-edited compose file, a changed .env, a
	// container quietly running a different image.
	Config bool `json:"config,omitempty"`

	// Route is set when the installed Caddy snippet differs from the one that
	// shipped with the release.
	Route bool `json:"route,omitempty"`

	Detail string `json:"detail,omitempty"`
}

// Any reports whether anything actually drifted.
func (d *Drift) Any() bool { return d != nil && (d.Config || d.Route) }

// RouteAction tells the agent what to do with a service's Caddy route.
type RouteAction string

const (
	RouteNone    RouteAction = "unchanged"
	RouteInstall RouteAction = "install"
	RouteUpdate  RouteAction = "update"
)

// DeployRequest asks the agent to activate a release that the CLI has already
// staged.
//
// Staging stays with the CLI because that is where the built bytes are.
// Everything after the commit point belongs to the agent, so that closing a
// laptop cannot leave a half-deployed service with nobody to roll it back.
type DeployRequest struct {
	Service string `json:"service"`
	Release string `json:"release"`

	// Spec is the service definition as YAML. The agent caches it so it can
	// keep observing, probing, and alerting on this service without a CLI.
	Spec string `json:"spec"`

	Route       string      `json:"route,omitempty"`
	RouteAction RouteAction `json:"route_action,omitempty"`

	Verify       bool   `json:"verify"`
	KeepReleases int    `json:"keep_releases,omitempty"`
	By           string `json:"by,omitempty"`
}

// RollbackRequest asks the agent to return a service to an earlier release.
type RollbackRequest struct {
	Service string `json:"service"`

	// To is the release to activate; empty means the previous one.
	To string `json:"to,omitempty"`
	By string `json:"by,omitempty"`
}

// JobState is where a job has got to.
type JobState string

const (
	JobPending   JobState = "pending"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
)

// Done reports whether the job has finished, either way.
func (s JobState) Done() bool { return s == JobSucceeded || s == JobFailed }

// Job phases, in the order they occur.
const (
	PhaseQueued   = "queued"
	PhaseActivate = "activate"
	PhaseRoute    = "route"
	PhaseVerify   = "verify"
	PhaseRollback = "rollback"
	PhaseFinalise = "finalise"
	PhaseDone     = "done"
)

// JobEvent is one line of progress.
type JobEvent struct {
	At      time.Time `json:"at"`
	Phase   string    `json:"phase"`
	Message string    `json:"message"`
}

// Job is a unit of work the agent owns from start to finish.
//
// The CLI starts one and then streams its events. If the CLI dies, the job
// carries on — that is the entire point of moving activation onto the host.
type Job struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Service string   `json:"service"`
	Release string   `json:"release"`
	From    string   `json:"from,omitempty"`
	State   JobState `json:"state"`
	Phase   string   `json:"phase"`

	Events []JobEvent `json:"events,omitempty"`

	Error      string `json:"error,omitempty"`
	RolledBack bool   `json:"rolled_back,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Job kinds.
const (
	KindDeploy   = "deploy"
	KindRollback = "rollback"
)

// Endpoint paths.
const (
	PathInfo      = "/v1/info"
	PathStatus    = "/v1/status"
	PathServices  = "/v1/services/"
	PathDeploy    = "/v1/deploy"
	PathRollback  = "/v1/rollback"
	PathJobs      = "/v1/jobs/"
	PathDrift     = "/v1/drift"
	PathConfig    = "/v1/config"
	PathAlerts    = "/v1/alerts"
	PathDashboard = "/v1/dashboard"
)

// MetricSample is one point on a resource chart.
//
// CPUPct is a share of the whole host — all cores together are 100 — because
// a percentage without a denominator answers nothing. Capacity carries the
// denominators for absolute display.
type MetricSample struct {
	At       time.Time `json:"at"`
	CPUPct   float64   `json:"cpu_pct"`
	MemBytes uint64    `json:"mem_bytes"`
}

// Capacity is a host's denominators: what 100% means there.
type Capacity struct {
	Cores    int    `json:"cores"`
	MemTotal uint64 `json:"mem_total_bytes"`
}

// AlertEvent is one firing episode of a rule, open while ResolvedAt is zero.
type AlertEvent struct {
	Rule           string    `json:"rule"`
	Subject        string    `json:"subject,omitempty"`
	FiredAt        time.Time `json:"fired_at"`
	ResolvedAt     time.Time `json:"resolved_at,omitzero"`
	DeliveryFailed bool      `json:"delivery_failed,omitempty"`
}

// DashboardResponse is everything the dashboard needs from one host in one
// round-trip. One fat response rather than four endpoints, because each call
// from the operator's machine costs an ssh exec — the shape of the wire
// follows the cost of the road.
type DashboardResponse struct {
	Host     string   `json:"host"`
	Capacity Capacity `json:"capacity"`

	Status StatusResponse `json:"status"`

	// Samples carry only points newer than the request's `since`, so a
	// steady-state poll moves a handful of numbers, not six hours of them.
	ServiceSamples map[string][]MetricSample `json:"service_samples,omitempty"`
	HostSamples    []MetricSample            `json:"host_samples,omitempty"`

	// AlertEvents is the full episode ring — bounded, and small enough that
	// windowing it is not worth a second cursor.
	AlertEvents []AlertEvent `json:"alert_events,omitempty"`

	// Deploys is each service's recorded history, newest first.
	Deploys map[string][]release.DeployRecord `json:"deploys,omitempty"`
}

// ConfigRequest carries the host-wide half of the configuration: notifiers and
// host-scoped alert rules, which have no service to hang off.
//
// Pushing it is what lets the agent alert entirely on its own — no central
// server, no dashboard, nobody's laptop open.
type ConfigRequest struct {
	// Spec is the fleet-level configuration as YAML.
	Spec string `json:"spec"`
}

// AlertsResponse reports which rules are currently firing.
type AlertsResponse struct {
	Host   string   `json:"host"`
	Firing []string `json:"firing"`
}
