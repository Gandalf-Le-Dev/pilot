// Package config defines Pilot's on-disk configuration: the fleet inventory
// (fleet.yaml) and one file per service (services/*.yaml).
//
// Parsing is deliberately permissive about *values* — enums and cross-references
// are checked by Validate, which reports Diagnostics carrying a file, a field
// path, and a hint. That way `pilot doctor` can render every problem in the
// config at once, rather than aborting on the first bad field.
package config

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runtime selects the adapter that knows how to stage and activate a service.
type Runtime string

const (
	RuntimeCompose Runtime = "compose"
	RuntimeSystemd Runtime = "systemd"
	RuntimeStatic  Runtime = "static"
)

var AllRuntimes = []Runtime{RuntimeCompose, RuntimeSystemd, RuntimeStatic}

// Manage controls whether Pilot may write to a service or only read from it.
// Stateful services (databases) use ManageObserve so that a fleet-wide deploy
// can never recreate them.
type Manage string

const (
	ManageDeploy  Manage = "deploy"
	ManageObserve Manage = "observe"
)

var AllManageModes = []Manage{ManageDeploy, ManageObserve}

// Fleet is the parsed contents of fleet.yaml plus every service loaded from
// the services/ directory alongside it.
type Fleet struct {
	Version   int                 `yaml:"version"`
	Defaults  Defaults            `yaml:"defaults"`
	Hosts     map[string]*Host    `yaml:"hosts"`
	Caddy     Caddy               `yaml:"caddy"`
	Notifiers map[string]Notifier `yaml:"notifiers"`

	// Alerts are host-wide rules, evaluated by every agent. Per-service rules
	// live on the service.
	Alerts []Alert `yaml:"alerts"`

	// NotifyDeploys announces finished deploys and rollbacks through the
	// configured notifiers. Nil means enabled.
	//
	// A pointer so "unset" and "explicitly false" differ: the default has to be
	// on, because a deploy nobody was told about is the case this exists for.
	NotifyDeploys *bool `yaml:"notify_deploys,omitempty"`

	// Populated by Load, not by YAML.
	Services map[string]*Service `yaml:"-"`
	Root     string              `yaml:"-"`
	File     string              `yaml:"-"`
}

type Defaults struct {
	User         string   `yaml:"user"`
	Port         int      `yaml:"port"`
	KeepReleases int      `yaml:"keep_releases"`
	Health       *Health  `yaml:"health"`
	Rollout      *Rollout `yaml:"rollout"`
}

type Caddy struct {
	Admin      string `yaml:"admin"`
	SnippetDir string `yaml:"snippet_dir"`
	Caddyfile  string `yaml:"caddyfile"`
}

// Notifier is one alert delivery target.
//
// Each is a single HTTP POST or a single exec — no retry queue, no templating.
// Anything needing either should be a `command` pointing at a program that does
// it properly, which is also how email is handled.
type Notifier struct {
	Type string `yaml:"type"`

	URL     string `yaml:"url"`
	Webhook string `yaml:"webhook"`

	// Command is the argv for a `command` notifier. The notification arrives
	// as JSON on its stdin.
	Command []string `yaml:"command"`
}

// Endpoint returns the URL this notifier posts to, whichever field carried it.
func (n Notifier) Endpoint() string {
	if n.URL != "" {
		return n.URL
	}
	return n.Webhook
}

// Host is one SSH-reachable machine.
type Host struct {
	Name    string   `yaml:"-"`
	Address string   `yaml:"address"`
	User    string   `yaml:"user"`
	Port    int      `yaml:"port"`
	Tags    []string `yaml:"tags"`
	SSH     SSH      `yaml:"ssh"`

	// Sudo runs every remote command through `sudo -n`.
	//
	// Pilot writes to /opt/pilot and /etc/caddy and drives systemd, none of
	// which an ordinary deploy user can do. Connecting as root instead is
	// often not an option — and is worse anyway — so the normal arrangement is
	// an unprivileged user with passwordless sudo.
	Sudo bool `yaml:"sudo"`
}

type SSH struct {
	ProxyJump    string `yaml:"proxy_jump"`
	IdentityFile string `yaml:"identity_file"`
}

// HasTag reports whether the host carries the given tag.
func (h *Host) HasTag(tag string) bool {
	return slices.Contains(h.Tags, tag)
}

// Service is one deployable workload, loaded from services/<name>.yaml.
type Service struct {
	Name    string   `yaml:"name"`
	Runtime Runtime  `yaml:"runtime"`
	Hosts   []string `yaml:"hosts"`
	Manage  Manage   `yaml:"manage"`

	Source  *Source           `yaml:"source"`
	Build   *Build            `yaml:"build"`
	Compose *Compose          `yaml:"compose"`
	Unit    *Unit             `yaml:"unit"`
	Env     map[string]string `yaml:"env"`
	Expose  *Expose           `yaml:"expose"`
	Health  *Health           `yaml:"health"`
	Rollout *Rollout          `yaml:"rollout"`
	Alerts  []Alert           `yaml:"alerts"`

	KeepReleases int `yaml:"keep_releases"`

	// Populated by Load, not by YAML.
	File string `yaml:"-"`
}

// Deployable reports whether Pilot is permitted to write to this service.
func (s *Service) Deployable() bool { return s.Manage != ManageObserve }

// EnvKeys returns environment variable names in sorted order, so rendered
// .env files are byte-stable across deploys and only change when the values do.
func (s *Service) EnvKeys() []string {
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type Source struct {
	Repo string `yaml:"repo"`
	Ref  string `yaml:"ref"`
	Path string `yaml:"path"`
}

// Build describes how an artifact is produced. It always runs on the operator's
// machine or in CI — never on a target host.
type Build struct {
	// Container image builds.
	Image      string `yaml:"image"`
	Dockerfile string `yaml:"dockerfile"`
	Context    string `yaml:"context"`

	// Command builds (static sites, Go binaries).
	Command string   `yaml:"command"`
	Output  []string `yaml:"output"`
}

type Compose struct {
	File    string `yaml:"file"`
	Project string `yaml:"project"`
}

// UnitKind distinguishes a long-running daemon from a task that runs and exits.
//
// The two differ in every method a runtime implements: a oneshot has no
// "running" state to observe, restarting it means running the job right now
// rather than picking up a new release, and health is "the last run succeeded
// recently" instead of "the process is up".
type UnitKind string

const (
	UnitService UnitKind = "service"
	UnitOneshot UnitKind = "oneshot"
)

var AllUnitKinds = []UnitKind{UnitService, UnitOneshot}

// Unit names the systemd unit that runs a service.
//
// Pilot adopts an existing unit; it does not write one. systemd's surface is
// enormous — KillMode, TimeoutStopSec, the sandboxing options — and modelling
// it in a second schema only produces a worse systemd. More practically, the
// unit file is where an operator records what their service needs in order to
// shut down safely, and a deploy tool that overwrites that is how a routine
// restart starts losing data.
//
// So the split is: something else writes the unit, Pilot ships releases and
// drives systemctl. It is the same bargain Pilot already makes with Docker —
// it expects a host that has it and never installs it.
//
// The cost is that the unit is not part of the release, so a rollback does not
// restore a hand-edited unit. Fingerprint hashes the unit file to compensate:
// Pilot detects the edit rather than owning the file.
type Unit struct {
	// Name is the unit systemctl acts on, e.g. "hopboxd.service".
	Name string `yaml:"name"`

	// Kind defaults to UnitService.
	Kind UnitKind `yaml:"kind"`

	// Timer schedules a oneshot, e.g. "backup.timer". Restarting the service
	// unit of a oneshot would run the job immediately, which a deploy did not
	// ask for, so the timer is what gets restarted instead.
	Timer string `yaml:"timer"`

	// Fresh bounds how old a oneshot's last success may be before the service
	// is reported degraded. A backup that quietly stopped running two months
	// ago is the exact failure this exists to catch.
	Fresh Duration `yaml:"fresh"`

	// Links are absolute paths on the host that should resolve into the live
	// release, e.g. "/usr/local/bin/hopboxd": "hopboxd".
	//
	// They point through `current`, never at a release directory, so the
	// symlink swap alone changes what they mean — and a rollback restores them
	// with no extra step and nothing to get wrong.
	Links map[string]string `yaml:"links"`

	// Precheck validates the staged release before anything swaps. It runs
	// with the release directory as its working directory, so `./hopboxd
	// --check` names the incoming binary rather than the live one.
	//
	// This is what separates "the deploy failed and nothing changed" from
	// "the deploy failed and the daemon is down".
	Precheck []string `yaml:"precheck"`
}

// IsOneshot reports whether the unit runs and exits rather than staying up.
func (u *Unit) IsOneshot() bool { return u.Kind == UnitOneshot }

// RestartTarget is the unit to restart so a new release takes effect, or ""
// when there is nothing to restart and the swap is the whole deploy.
func (u *Unit) RestartTarget() string {
	if u.IsOneshot() {
		return u.Timer
	}
	return u.Name
}

// LinkPaths returns the link destinations in sorted order, so scripts and
// fingerprints are byte-stable across runs.
func (u *Unit) LinkPaths() []string {
	paths := make([]string, 0, len(u.Links))
	for p := range u.Links {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// Expose declares the service's front door. Pilot renders it into a Caddy
// snippet at /etc/caddy/pilot.d/<service>.caddy.
type Expose struct {
	Domains  []string `yaml:"domains"`
	Path     string   `yaml:"path"`
	Upstream int      `yaml:"upstream"`

	// Allow restricts who may reach the service, as CIDR blocks. Everyone
	// else gets a closed connection rather than a 403, so the service does not
	// advertise its own existence.
	//
	// This is a Caddy-level control and only as strong as Caddy being the sole
	// route in — a container publishing on 0.0.0.0 bypasses it entirely, which
	// is why Pilot binds published ports to loopback.
	Allow    []string      `yaml:"allow"`
	Static   *StaticExpose `yaml:"static"`
	Timeouts *Timeouts     `yaml:"timeouts"`
	Verify   bool          `yaml:"verify"`
	Raw      string        `yaml:"raw"`
}

// Restricted reports whether this route is limited to particular clients.
func (e *Expose) Restricted() bool { return e != nil && len(e.Allow) > 0 }

// TailnetCIDRs are Tailscale's address ranges, the common case for `allow`.
var TailnetCIDRs = []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}

// IsStatic reports whether this route is served from disk rather than proxied.
func (e *Expose) IsStatic() bool { return e.Static != nil }

// SiteAddresses returns the Caddyfile site addresses this route claims, which
// is what collision detection compares across services.
func (e *Expose) SiteAddresses() []string {
	path := strings.TrimSuffix(e.Path, "/*")
	addrs := make([]string, 0, len(e.Domains))
	for _, d := range e.Domains {
		addrs = append(addrs, d+path)
	}
	return addrs
}

type StaticExpose struct {
	SPA     bool                         `yaml:"spa"`
	Index   string                       `yaml:"index"`
	Headers map[string]map[string]string `yaml:"headers"`

	// Overlay names directories whose files are carried forward from the
	// previous release by hardlink, so a page loaded just before a swap can
	// still fetch its hash-named assets instead of 404ing.
	//
	// Scoped to named directories rather than the whole tree on purpose: a
	// blanket carry-forward would mean deleted pages never disappear. Set it
	// to [] to turn the behaviour off entirely.
	Overlay []string `yaml:"overlay"`
}

// DefaultOverlayDirs is used when `overlay` is not specified. It matches where
// Vite and most bundlers put hashed assets, and is harmlessly a no-op for sites
// that have no such directory.
var DefaultOverlayDirs = []string{"assets"}

// OverlayDirs returns the directories to carry forward. An unset value gets the
// default; an explicitly empty list disables the behaviour.
func (s *StaticExpose) OverlayDirs() []string {
	if s.Overlay == nil {
		return DefaultOverlayDirs
	}
	return s.Overlay
}

// UnmarshalYAML accepts both `static: true` and `static: {spa: true}`, because
// the bare-bool form reads better for a plain file server with no options.
func (s *StaticExpose) UnmarshalYAML(b []byte) error {
	switch strings.TrimSpace(string(b)) {
	case "true":
		return nil
	case "false", "null", "~", "":
		return nil
	}
	type raw StaticExpose
	var r raw
	if err := unmarshalStrict(b, &r); err != nil {
		return err
	}
	*s = StaticExpose(r)
	return nil
}

type Timeouts struct {
	Read  Duration `yaml:"read"`
	Write Duration `yaml:"write"`
	Dial  Duration `yaml:"dial"`
}

// Health describes how to tell whether a service is actually working. Exactly
// one probe kind should be set.
type Health struct {
	HTTP    *HTTPProbe `yaml:"http"`
	TCP     *TCPProbe  `yaml:"tcp"`
	Exec    *ExecProbe `yaml:"exec"`
	Docker  bool       `yaml:"docker"`
	Systemd bool       `yaml:"systemd"`

	Timeout  Duration `yaml:"timeout"`
	Interval Duration `yaml:"interval"`
}

// Probes reports how many probe kinds are configured.
func (h *Health) Probes() int {
	n := 0
	for _, set := range []bool{h.HTTP != nil, h.TCP != nil, h.Exec != nil, h.Docker, h.Systemd} {
		if set {
			n++
		}
	}
	return n
}

type HTTPProbe struct {
	URL    string `yaml:"url"`
	Expect int    `yaml:"expect"`
}

type TCPProbe struct {
	Addr string `yaml:"addr"`
}

type ExecProbe struct {
	Cmd []string `yaml:"cmd"`
}

type Rollout struct {
	Strategy     string   `yaml:"strategy"`
	Concurrency  int      `yaml:"concurrency"`
	MaxUnhealthy int      `yaml:"max_unhealthy"`
	PauseBetween Duration `yaml:"pause_between"`

	// Service names the compose service that receives traffic. Blue-green
	// needs it because it must know which container's port to publish.
	Service string `yaml:"service"`

	// Ports are the host ports for the two colors. Declared, never inferred:
	// a silent port collision is an outage, and one Pilot can catch at
	// config-load time instead.
	Ports []int `yaml:"ports"`

	// Drain is how long the outgoing color keeps running after traffic moves,
	// so in-flight requests finish against the version that started them.
	Drain Duration `yaml:"drain"`
}

const (
	StrategyRecreate  = "recreate"
	StrategyBlueGreen = "blue-green"
)

// AllStrategies is every rollout strategy this build supports.
var AllStrategies = []string{StrategyRecreate, StrategyBlueGreen}

// DefaultDrain is how long the old color lingers after the flip.
const DefaultDrain = Duration(30_000_000_000) // 30s

// IsBlueGreen reports whether this service flips between two live stacks.
func (r *Rollout) IsBlueGreen() bool { return r != nil && r.Strategy == StrategyBlueGreen }

// DrainOrDefault is how long the outgoing color lingers after the flip.
//
// The zero value is left alone in the parsed config so validation can tell
// whether the operator chose it; this is where the default is actually applied.
func (r *Rollout) DrainOrDefault() Duration {
	if r == nil || r.Drain.IsZero() {
		return DefaultDrain
	}
	return r.Drain
}

// Color is which of the two stacks is meant.
type Color string

const (
	ColorBlue  Color = "blue"
	ColorGreen Color = "green"
)

// Other returns the color a deploy would move to.
func (c Color) Other() Color {
	if c == ColorGreen {
		return ColorBlue
	}
	return ColorGreen
}

// AllColors is both colors, in the order ports are assigned.
var AllColors = []Color{ColorBlue, ColorGreen}

// PortFor returns the host port assigned to a color.
func (r *Rollout) PortFor(c Color) int {
	if len(r.Ports) < 2 {
		return 0
	}
	if c == ColorGreen {
		return r.Ports[1]
	}
	return r.Ports[0]
}

// Alert is one rule. `when` is the condition, `for` is how long it must hold
// before firing, and `cooldown` is how long a firing rule stays quiet before
// repeating itself.
type Alert struct {
	When     string   `yaml:"when"`
	For      Duration `yaml:"for"`
	Cooldown Duration `yaml:"cooldown"`
	Notify   []string `yaml:"notify"`
}

// Duration is a time.Duration that unmarshals from YAML scalars like "90s".
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }
func (d Duration) String() string          { return time.Duration(d).String() }
func (d Duration) IsZero() bool            { return d == 0 }

func (d *Duration) UnmarshalYAML(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"'`)
	if s == "" || s == "null" || s == "~" {
		return nil
	}
	// A bare number is read as seconds, which is what people expect from
	// `timeout: 30` in a config file.
	if n, err := strconv.Atoi(s); err == nil {
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: want a value like 30s, 5m, or 1h", s)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// FleetConfig is the host-wide half of the configuration: notifiers and
// host-scoped rules, which have no service to hang off.
//
// It lives here rather than in the agent package because the agent parses it
// strictly, which makes it part of the CLI/agent contract — and because
// SchemaFingerprint can only guard schemas it can reach. Defined next to the
// agent it would have been a field set nobody was checking, which is precisely
// the hole `expose.allow` went through.
type FleetConfig struct {
	Notifiers map[string]Notifier `yaml:"notifiers"`
	Alerts    []Alert             `yaml:"alerts"`

	// NotifyDeploys sends a message through the configured notifiers whenever a
	// deploy or rollback finishes. Nil means enabled.
	//
	// A pointer so that "unset" and "explicitly false" are distinguishable: the
	// default has to be on, because a deploy nobody was told about is the case
	// this exists for.
	NotifyDeploys *bool `yaml:"notify_deploys,omitempty"`
}

// DeployNotificationsEnabled reports whether finished deploys should notify.
func (f FleetConfig) DeployNotificationsEnabled() bool {
	return f.NotifyDeploys == nil || *f.NotifyDeploys
}

// HasSecretRef reports whether a value contains a `${scheme:...}` reference.
//
// Validation runs before resolution, so anything checking the *shape* of a
// value — a URL, a port, a path — has to accept a reference it cannot see
// through yet. Getting this wrong does not merely produce a spurious error: it
// forces the value to be written literally, and the values worth referencing
// are the ones worth keeping out of a repository.
func HasSecretRef(v string) bool { return strings.Contains(v, "${") }
