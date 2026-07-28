// Package app holds the wiring shared by every Pilot command: finding the
// fleet configuration, opening connections, and choosing a runtime adapter.
//
// This is the composition root. Keeping it here means the packages underneath
// stay unaware of each other — the runtimes don't know about config discovery,
// and config doesn't know about ssh.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/remote"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime/compose"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime/static"
	"github.com/Gandalf-Le-Dev/pilot/internal/secrets"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/ssh"
)

// App carries the loaded fleet and the connections opened so far.
type App struct {
	Root  string
	Fleet *config.Fleet
	Diags config.Diagnostics

	JSON   bool
	Layout release.Layout

	mu      sync.Mutex
	clients map[string]*ssh.Client
}

// Load finds and parses the fleet configuration.
//
// Validation errors are returned as diagnostics rather than a hard failure, so
// `pilot doctor` can report all of them at once. Commands that mutate anything
// must call RequireValid first.
func Load(root string) (*App, error) {
	resolved, err := Discover(root)
	if err != nil {
		return nil, err
	}

	// Load the fleet's .env before anything reads the environment, so
	// `pilot deploy` is a command you type rather than one you prefix.
	if _, err := secrets.LoadDotenv(resolved); err != nil {
		return nil, err
	}

	fleet, diags, err := config.Load(resolved)
	if err != nil {
		return nil, err
	}

	return &App{
		Root:    resolved,
		Fleet:   fleet,
		Diags:   diags,
		Layout:  release.NewLayout(""),
		clients: map[string]*ssh.Client{},
	}, nil
}

// Discover walks up from dir looking for a fleet.yaml, so Pilot can be run from
// anywhere inside the configuration repo.
func Discover(dir string) (string, error) {
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	for cur := abs; ; {
		if _, err := os.Stat(filepath.Join(cur, config.FleetFile)); err == nil {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", fmt.Errorf("no %s found in %s or any parent directory\nrun `pilot init` to create one", config.FleetFile, abs)
}

// RequireValid refuses to continue when the configuration has errors.
func (a *App) RequireValid() error {
	if err := a.Diags.Err(); err != nil {
		return fmt.Errorf("%w\n\nrun `pilot doctor` for the full picture", err)
	}
	return nil
}

// ControlDir is where multiplexed ssh sockets live.
func ControlDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pilot-cm")
	}
	return filepath.Join(home, ".pilot", "cm")
}

// Client opens (or reuses) a connection to a host.
//
// Connections are cached for the process lifetime and multiplexed by ssh
// itself, so a fleet-wide command costs one handshake per host rather than one
// per command.
func (a *App) Client(host string) (*ssh.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if c, ok := a.clients[host]; ok {
		return c, nil
	}

	h, ok := a.Fleet.Hosts[host]
	if !ok {
		return nil, fmt.Errorf("no such host %q in %s", host, config.FleetFile)
	}

	cfg := ssh.Config{
		Name:           h.Name,
		Address:        h.Address,
		User:           h.User,
		Port:           h.Port,
		IdentityFile:   h.SSH.IdentityFile,
		ControlDir:     ControlDir(),
		BatchMode:      true,
		ConnectTimeout: ssh.DefaultConnectTimeout,
		Sudo:           h.Sudo,
	}
	if jump := h.SSH.ProxyJump; jump != "" {
		// A proxy_jump names another fleet host; resolve it to an ssh
		// destination so operators write fleet names, not addresses.
		if jh, ok := a.Fleet.Hosts[jump]; ok {
			jumpCfg := ssh.Config{Address: jh.Address, User: jh.User, Port: jh.Port}
			cfg.ProxyJump = jumpCfg.Target()
		} else {
			cfg.ProxyJump = jump
		}
	}

	c, err := ssh.New(cfg)
	if err != nil {
		return nil, err
	}
	a.clients[host] = c
	return c, nil
}

// ConnectAll opens connections to every named host, returning only those that
// answered. Unreachable hosts are reported by the caller as one finding each,
// rather than as a failure per subsequent check.
func (a *App) ConnectAll(ctx context.Context, hosts []string) map[string]*ssh.Client {
	out := map[string]*ssh.Client{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, name := range hosts {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			c, err := a.Client(name)
			if err != nil {
				return
			}
			if res, err := c.Run(ctx, "true"); err != nil || !res.OK() {
				return
			}
			mu.Lock()
			out[name] = c
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	return out
}

// Close tears down every open connection.
func (a *App) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.clients {
		_ = c.Close()
	}
	a.clients = map[string]*ssh.Client{}
}

// Agent returns a client for the agent on a host.
//
// It does not check whether one is actually there; callers that can work
// without an agent should use AgentOrNil and fall back to driving the host
// directly, which is the degraded mode the design calls for.
func (a *App) Agent(host string) (*remote.Client, error) {
	c, err := a.Client(host)
	if err != nil {
		return nil, err
	}
	return remote.New(c, host, a.Layout), nil
}

// AgentOrNil returns a usable agent client, or nil when the host has none.
//
// A missing or version-skewed agent is not an error: Pilot predates the agent
// and still works without one. The caller reports the degradation and carries
// on over SSH.
func (a *App) AgentOrNil(ctx context.Context, host string) (*remote.Client, string) {
	rc, err := a.Agent(host)
	if err != nil {
		return nil, err.Error()
	}
	if _, err := rc.Check(ctx); err != nil {
		return nil, err.Error()
	}
	return rc, ""
}

// CaddyPaths returns the fleet's Caddy locations.
func (a *App) CaddyPaths() caddy.Paths {
	return caddy.Paths{
		Caddyfile:  a.Fleet.Caddy.Caddyfile,
		SnippetDir: a.Fleet.Caddy.SnippetDir,
		Admin:      a.Fleet.Caddy.Admin,
	}
}

// RuntimeFor selects the adapter for a service.
//
// This switch is the only place that knows the full set of runtimes; adding one
// means implementing the interface and adding a case here.
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

// Target builds the (service, host) pair a runtime acts on.
func (a *App) Target(s *config.Service, host string, releaseID string) (*runtime.Target, error) {
	h, ok := a.Fleet.Hosts[host]
	if !ok {
		return nil, fmt.Errorf("no such host %q", host)
	}
	c, err := a.Client(host)
	if err != nil {
		return nil, err
	}
	return &runtime.Target{
		Service: s,
		Host:    h,
		Layout:  a.Layout,
		Client:  c,
		Release: releaseID,
	}, nil
}
