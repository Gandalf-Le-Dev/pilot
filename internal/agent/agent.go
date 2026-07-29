// Package agent is the daemon that runs on each host.
//
// Its reason to exist is that some things can only be done correctly by a
// process that is actually there: verifying a deploy took, rolling it back when
// it didn't, noticing drift, and firing an alert at 3am with nobody's laptop
// open. Everything the CLI could do perfectly well over SSH stays in the CLI.
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime/compose"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime/static"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/local"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// CacheDir holds the service definitions the CLI has pushed, relative to the
// Pilot root. Keeping them on disk is what lets the agent go on observing,
// probing, and alerting across a reboot with no CLI in sight.
const CacheDir = "fleet-cache"

// Agent is the daemon's state.
type Agent struct {
	Layout release.Layout
	Exec   transport.Executor
	Caddy  caddy.Paths
	Host   string
	Build  string

	StartedAt time.Time

	mu       sync.RWMutex
	services map[string]*config.Service
	drift    map[string]*driftRecord
	samples  map[string][]Sample
	fleet    *FleetConfig

	jobs   *JobStore
	alerts *alert.Engine
}

type driftRecord struct {
	detectedAt time.Time
	release    string
	config     bool
	route      bool
	detail     string
}

// Options configures a new agent.
type Options struct {
	Root      string
	Host      string
	Build     string
	Caddyfile string
	Snippets  string
	Admin     string
}

// New returns an agent and loads any cached service definitions.
func New(opts Options) (*Agent, error) {
	host := opts.Host
	if host == "" {
		host, _ = os.Hostname()
	}

	a := &Agent{
		Layout:    release.NewLayout(opts.Root),
		Exec:      local.New(host),
		Host:      host,
		Build:     opts.Build,
		StartedAt: time.Now().UTC(),
		services:  map[string]*config.Service{},
		drift:     map[string]*driftRecord{},
		jobs:      NewJobStore(),
		alerts:    alert.NewEngine(host, nil),
		Caddy: caddy.Paths{
			Caddyfile:  orDefault(opts.Caddyfile, config.DefaultCaddyfile),
			SnippetDir: orDefault(opts.Snippets, config.DefaultSnippetDir),
			Admin:      orDefault(opts.Admin, config.DefaultCaddyAdmin),
		},
	}

	a.alerts.OnError = func(rule, notifier string, err error) {
		logf("alert %q: notifier %q failed: %v", rule, notifier, err)
	}

	if err := a.loadCache(); err != nil {
		return nil, err
	}
	if err := a.loadFleetConfig(); err != nil {
		return nil, err
	}
	return a, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// cacheDir is where service definitions live on the host.
func (a *Agent) cacheDir() string { return filepath.Join(a.Layout.Root, CacheDir) }

// loadCache reads every cached service definition.
//
// A definition that fails to parse is skipped with a log line rather than
// aborting startup: one bad file must not take the whole agent down and with it
// the monitoring of every other service on the box.
func (a *Agent) loadCache() error {
	entries, err := os.ReadDir(a.cacheDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(a.cacheDir(), e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			logf("skipping %s: %v", path, err)
			continue
		}
		s, err := config.ParseService(raw, e.Name())
		if err != nil {
			logf("skipping %s: %v", path, err)
			continue
		}
		a.services[s.Name] = s
	}
	return nil
}

// PutService caches a service definition, replacing any previous one.
//
// Parsing is strict, so a field this agent does not know is refused rather
// than ignored — silently dropping `expose.allow` would serve a route without
// its address restriction and call the deploy a success.
func (a *Agent) PutService(spec string) (*config.Service, error) {
	s, err := config.ParseService([]byte(spec), "pushed")
	if err != nil {
		// A rejected field usually means the CLI is newer than this agent.
		// The handshake should have caught that first — proto.Version now
		// moves with the config schema — so reaching here means something
		// slipped past it, and naming the fix beats leaving an operator with
		// a YAML error about a field they never wrote.
		if strings.Contains(err.Error(), "unknown field") {
			return nil, fmt.Errorf("%w\n\nthis agent (protocol %d) does not know that field, so it was "+
				"probably\nwritten by a newer pilot — update the agent with:  pilot bootstrap %s",
				err, proto.Version, a.Host)
		}
		return nil, err
	}
	if s.Name == "" {
		return nil, fmt.Errorf("service definition has no name")
	}

	if err := os.MkdirAll(a.cacheDir(), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(a.cacheDir(), s.Name+".yaml")
	if err := release.WriteFileAtomic(path, []byte(spec), 0o644); err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.services[s.Name] = s
	a.mu.Unlock()
	return s, nil
}

// Service returns a cached definition.
func (a *Agent) Service(name string) (*config.Service, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.services[name]
	return s, ok
}

// ServiceNames returns the cached services in sorted order.
func (a *Agent) ServiceNames() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()

	out := make([]string, 0, len(a.services))
	for n := range a.services {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ForgetService drops a cached definition.
func (a *Agent) ForgetService(name string) error {
	a.mu.Lock()
	delete(a.services, name)
	delete(a.drift, name)
	a.mu.Unlock()
	a.alerts.Forget(name)

	err := os.Remove(filepath.Join(a.cacheDir(), name+".yaml"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Jobs exposes the job store.
func (a *Agent) Jobs() *JobStore { return a.jobs }

// Target builds the runtime target for a service on this host.
//
// The host is synthesised rather than read from a fleet file: the agent only
// ever acts on the machine it is running on, so there is nothing to look up.
func (a *Agent) Target(s *config.Service, releaseID string) *runtime.Target {
	return &runtime.Target{
		Service: s,
		Host:    &config.Host{Name: a.Host, Address: a.Host},
		Layout:  a.Layout,
		Client:  a.Exec,
		Release: releaseID,
	}
}

// RuntimeFor selects the adapter for a service.
func RuntimeFor(s *config.Service) (runtime.Runtime, error) {
	switch s.Runtime {
	case config.RuntimeCompose:
		return compose.New(), nil
	case config.RuntimeStatic:
		return static.New(), nil
	case config.RuntimeSystemd:
		return nil, fmt.Errorf("the systemd runtime is not implemented in this build (service %q)", s.Name)
	}
	return nil, fmt.Errorf("service %q has unknown runtime %q", s.Name, s.Runtime)
}

// Observe reports what one service is actually doing.
func (a *Agent) Observe(ctx context.Context, name string) (runtime.Observation, error) {
	s, ok := a.Service(name)
	if !ok {
		return runtime.Observation{}, fmt.Errorf("no such service %q on this host", name)
	}
	rt, err := RuntimeFor(s)
	if err != nil {
		return runtime.Observation{}, err
	}
	return rt.Observe(ctx, a.Target(s, ""))
}

// Drift returns the recorded drift for a service, if any.
func (a *Agent) Drift(name string) *driftRecord {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.drift[name]
}

func (a *Agent) setDrift(name string, d *driftRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d == nil {
		delete(a.drift, name)
		return
	}
	a.drift[name] = d
}

// logf writes a daemon log line. systemd captures stderr into the journal, so
// there is no log file to rotate and no logging configuration to get wrong.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pilotd: "+format+"\n", args...)
}
