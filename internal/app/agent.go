package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent"
	"github.com/Gandalf-Le-Dev/pilot/internal/agent/install"
	"github.com/Gandalf-Le-Dev/pilot/internal/agent/remote"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// AgentSync describes where an agent binary should come from.
//
// Version and ModuleDir are the CLI's own, passed in rather than read here
// because only main knows them — and the whole point of the source rules is
// that the agent is the one belonging to *this* build.
type AgentSync struct {
	Version   string
	ModuleDir string

	// Log receives progress. Nil discards it, which is what the self-healing
	// path inside a deploy wants when it is already printing its own lines.
	Log func(string, ...any)
}

// AgentSyncResult describes what an install did.
type AgentSyncResult struct {
	Arch      install.Arch
	Origin    string
	Info      *proto.Info
	Notifiers int
	Rules     int
}

// SyncAgent installs the agent belonging to this CLI on a host and waits for it
// to answer.
//
// Idempotent, and the single implementation behind three callers: `pilot
// bootstrap` preparing a new host, `pilot agent upgrade` bringing one up to
// date, and a deploy repairing a version-skewed agent rather than silently
// proceeding without one.
//
// That last caller is why this exists as a function at all. A skewed agent used
// to make the deploy fall back to driving the host over SSH, which quietly
// gives up the automatic rollback the agent is there to provide — a safety
// regression triggered by the unrelated act of upgrading the CLI.
func (a *App) SyncAgent(ctx context.Context, host string, s AgentSync) (*AgentSyncResult, error) {
	logf := s.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	client, err := a.Client(host)
	if err != nil {
		return nil, err
	}

	arch, err := install.DetectArch(ctx, client)
	if err != nil {
		return nil, err
	}

	src := install.Source{Version: s.Version, ModuleDir: s.ModuleDir}
	binary, origin, cleanup, err := src.Resolve(ctx, arch)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	res := &AgentSyncResult{Arch: arch, Origin: origin}
	logf("agent for linux/%s (%s)", arch, origin)

	if _, err := install.Install(ctx, client, binary, install.Options{
		Host: host, Layout: a.Layout, Caddy: a.CaddyPaths(),
	}); err != nil {
		return nil, err
	}

	// Confirm the daemon actually came up and speaks our protocol. Reporting
	// success on the strength of `systemctl restart` returning 0 would be a
	// lie: the unit can start and the process exit a second later.
	rc := remote.New(client, host, a.Layout)
	info, err := WaitForAgent(ctx, rc)
	if err != nil {
		// A skewed agent did come up — saying it did not sends someone to
		// journalctl to read healthy startup logs. It means the binary just
		// installed was not the one this pilot was built from.
		var skew *remote.SkewError
		if errors.As(err, &skew) {
			return nil, fmt.Errorf("the agent that started speaks protocol %d, but this pilot speaks %d\n"+
				"the binary installed (%s) is not the one this build expects",
				skew.Agent, skew.Expected, origin)
		}
		return nil, fmt.Errorf("the agent was installed but did not come up: %w\n"+
			"check it with:  ssh %s journalctl -u pilotd -n 50 --no-pager", err, host)
	}
	res.Info = info
	logf("agent running (build %s, protocol %d)", info.Build, info.Protocol)

	// Push the host-wide half of the configuration. Without it the agent can
	// evaluate rules but has nowhere to send them, and alerting silently does
	// nothing — which is the worst possible failure mode for alerting.
	spec, err := a.FleetConfigSpec()
	if err != nil {
		return nil, err
	}
	if err := rc.PutConfig(ctx, spec); err != nil {
		return nil, fmt.Errorf("installing alert configuration: %w", err)
	}
	res.Notifiers = len(a.Fleet.Notifiers)
	res.Rules = len(a.Fleet.Alerts)
	if res.Notifiers > 0 {
		logf("%d notifier(s) and %d host rule(s) installed", res.Notifiers, res.Rules)
	}
	return res, nil
}

// FleetConfigSpec renders the host-wide configuration the agent needs in order
// to alert on its own: notifiers and host-scoped rules.
func (a *App) FleetConfigSpec() (string, error) {
	body, err := yaml.Marshal(agent.FleetConfig{
		Notifiers: a.Fleet.Notifiers,
		Alerts:    a.Fleet.Alerts,
	})
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// WaitForAgent polls until the daemon answers, since systemd returns as soon as
// it has forked rather than when the process is ready.
func WaitForAgent(ctx context.Context, rc *remote.Client) (*proto.Info, error) {
	deadline := time.Now().Add(15 * time.Second)
	var last error

	for time.Now().Before(deadline) {
		info, err := rc.Check(ctx)
		if err == nil {
			return info, nil
		}
		last = err

		// A protocol mismatch will not resolve itself by waiting.
		var skew *remote.SkewError
		if errors.As(err, &skew) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, last
}

// AgentState is why a host has no usable agent.
type AgentState int

const (
	// AgentReady means the agent answered and matches this CLI.
	AgentReady AgentState = iota

	// AgentSkewed means an agent is running but speaks another protocol. It is
	// repairable in place, which is what separates it from the rest.
	AgentSkewed

	// AgentAbsent means nothing answered on the socket.
	AgentAbsent

	// AgentUnreachable means the host itself could not be contacted.
	AgentUnreachable
)

// InspectAgent reports whether a host's agent is usable, and if not, whether
// the problem is one Pilot can fix by reinstalling.
//
// AgentOrNil flattens every one of these into "no agent", which is what let a
// version skew masquerade as an absent daemon and downgrade a deploy in
// silence. Callers that care about the difference use this.
func (a *App) InspectAgent(ctx context.Context, host string) (*remote.Client, AgentState, error) {
	rc, err := a.Agent(host)
	if err != nil {
		return nil, AgentUnreachable, err
	}
	if _, err := rc.Check(ctx); err != nil {
		var skew *remote.SkewError
		if errors.As(err, &skew) {
			return rc, AgentSkewed, err
		}
		var missing *remote.NotRunningError
		if errors.As(err, &missing) {
			return nil, AgentAbsent, err
		}
		return nil, AgentUnreachable, err
	}
	return rc, AgentReady, nil
}
