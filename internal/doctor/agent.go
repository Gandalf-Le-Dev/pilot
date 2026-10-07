package doctor

import (
	"context"
	"fmt"
)

// AgentReport describes one host's agent.
type AgentReport struct {
	// Installed is false when nothing answered on the socket.
	Installed bool

	// Skewed marks an agent that is running but speaks a different protocol
	// version than this CLI. Repairable in place, which is what separates it
	// from an absent one.
	Skewed bool

	Build       string
	Protocol    int
	Expected    int
	Unreachable bool
	Detail      string

	// ConfigDigest is what the agent says its cached fleet config is;
	// WantDigest is what this fleet would push now. They differ when
	// fleet.yaml changed and nobody pushed it.
	ConfigDigest string
	WantDigest   string
}

// Agents is how doctor asks about agents and repairs them.
//
// An interface rather than a direct call because installing an agent needs the
// release-download and build-from-source rules, which live next to the CLI's own
// version — and doctor has no business importing any of that. The command wires
// an implementation in; nil simply disables the check.
type Agents interface {
	Status(ctx context.Context, host string) AgentReport
	Upgrade(ctx context.Context, host string) error

	// PushConfig sends the host-wide config through the agent's existing
	// config endpoint, without reinstalling anything.
	PushConfig(ctx context.Context, host string) error
}

// checkAgents reports agents that are missing or version-skewed.
//
// Skew earns a check of its own because it used to surface only as a failure
// part-way through some later command, and because the consequence is quiet: a
// deploy to a skewed host silently proceeded without the daemon, giving up the
// automatic rollback that is the agent's entire purpose. Something that costs
// you a safety net should be visible before it costs you one.
func checkAgents(ctx context.Context, env *Env) []Finding {
	if env.Agents == nil {
		return nil
	}

	var out []Finding
	for _, host := range env.Fleet.HostNames() {
		if env.Client(host) == nil {
			continue // unreachable hosts are already reported once
		}

		rep := env.Agents.Status(ctx, host)
		switch {
		case rep.Unreachable:
			continue

		case rep.Skewed:
			out = append(out, Finding{
				Status: StatusWarn, Scope: ScopeHost, Host: host,
				Title: fmt.Sprintf("agent speaks protocol %d, this pilot speaks %d", rep.Protocol, rep.Expected),
				Detail: "Pilot will not drive a deploy through an agent it cannot understand. " +
					"Until this is fixed, a deploy to this host runs without one — losing the " +
					"automatic rollback that would undo a failed release.",
				Hint:    "pilot agent upgrade " + host,
				Fix:     func(ctx context.Context) error { return env.Agents.Upgrade(ctx, host) },
				FixDesc: "install the agent matching this pilot",
			})

		case !rep.Installed:
			// Deliberately not auto-fixed. Replacing a running agent is a
			// repair; putting one on a host that never had one is setup, and
			// setup belongs to an explicit command.
			out = append(out, Finding{
				Status: StatusWarn, Scope: ScopeHost, Host: host,
				Title:  "no agent installed",
				Detail: "Deploys still work, driven over SSH, but without health-verified rollback, drift detection, or alerts.",
				Hint:   "pilot bootstrap " + host,
			})

		default:
			out = append(out, Finding{
				Status: StatusOK, Scope: ScopeHost, Host: host,
				Title: fmt.Sprintf("agent %s (protocol %d)", rep.Build, rep.Protocol),
			})
			if f := staleConfig(env, host, rep); f != nil {
				out = append(out, *f)
			}
		}
	}
	return out
}

// staleConfig reports an agent working from an older host-wide config than
// the fleet's. The agent is fine; the push that should have followed the edit
// to fleet.yaml never happened, so the fix is that push and nothing more.
func staleConfig(env *Env, host string, rep AgentReport) *Finding {
	if rep.WantDigest == "" || rep.ConfigDigest == rep.WantDigest {
		return nil
	}
	have := rep.ConfigDigest
	if have == "" {
		have = "none"
	}
	return &Finding{
		Status: StatusWarn, Scope: ScopeHost, Host: host,
		Title: "fleet config on the agent is stale",
		Detail: fmt.Sprintf("the agent holds %s, the fleet would push %s. Until it is pushed, this host "+
			"alerts with its old notifiers and rules and reports its old list to the status page.", have, rep.WantDigest),
		Hint:    "pilot doctor --fix, or pilot agent upgrade " + host,
		Fix:     func(ctx context.Context) error { return env.Agents.PushConfig(ctx, host) },
		FixDesc: "push the fleet config to the agent",
	}
}
