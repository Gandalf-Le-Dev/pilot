package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/remote"
	"github.com/Gandalf-Le-Dev/pilot/internal/app"
	"github.com/Gandalf-Le-Dev/pilot/internal/doctor"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// newAgentCmd groups the operations that act on the agent itself rather than on
// a service.
//
// `pilot bootstrap` could already do this, and saying so was the whole problem:
// "bootstrap" means prepare a new host, so telling someone to re-run it in
// order to update software reads like an instruction to start over. The work is
// identical; the name is what was wrong.
func newAgentCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Inspect and update the pilotd agents on your hosts",
		Long: "The agent owns everything after a deploy's commit point: health\n" +
			"verification, automatic rollback, drift detection, and alerting.\n\n" +
			"An agent is tied to the Pilot release that installed it, so upgrading the\n" +
			"CLI leaves your agents behind. Pilot will not talk to a mismatched agent —\n" +
			"it says so rather than guessing — and a deploy repairs one on the spot.",
	}
	cmd.AddCommand(
		annotate(newAgentUpgradeCmd(g), jsonNone),
		annotate(newAgentStatusCmd(g), jsonStructured),
	)
	return cmd
}

func newAgentUpgradeCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade [host|@tag]",
		Short: "Install the agent matching this Pilot on one or more hosts",
		Long: "Installs the pilotd belonging to this Pilot release, restarts it, and waits\n" +
			"for it to answer. Idempotent — running it against an already-current host is\n" +
			"a no-op you can repeat safely.\n\n" +
			"With no argument, every host in the fleet. Only hosts that actually need it\n" +
			"are touched.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var selector string
			if len(args) == 1 {
				selector = args[0]
			}
			return runAgentUpgrade(cmd.Context(), g, selector)
		},
	}
}

func runAgentUpgrade(ctx context.Context, g *globals, selector string) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.RequireValid(); err != nil {
		return err
	}

	hosts, err := selectHosts(a, selector)
	if err != nil {
		return err
	}

	fmt.Println()
	var failed, changed int

	for _, host := range hosts {
		// Skip hosts already running this exact build, so a fleet-wide run is
		// cheap and its output says what it actually did.
		//
		// The comparison is the build, not just the protocol. Matching
		// protocols mean the two can talk, which is what a deploy needs before
		// it will trust the agent — but they do not mean the agent is running
		// the same code, and a fix released in the CLI is no use sitting on
		// your laptop. Someone who types `agent upgrade` is asking for the
		// agent to match, so protocol equality is too weak a reason to decline.
		if rc, state, _ := a.InspectAgent(ctx, host); state == app.AgentReady {
			if info, err := rc.Check(ctx); err == nil && info.Build == version {
				fmt.Printf("  ✔ %s already runs %s\n", host, version)
				continue
			}
		}

		fmt.Printf("  %s\n", host)
		if _, err := a.SyncAgent(ctx, host, app.AgentSync{
			Version:   version,
			ModuleDir: moduleDir(),
			Log:       func(f string, args ...any) { fmt.Printf("    ✔ "+f+"\n", args...) },
		}); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "    ✖ %v\n", err)
			continue
		}
		changed++
	}

	fmt.Println()
	if failed > 0 {
		return exitWith(1)
	}
	if changed == 0 {
		fmt.Printf("  every agent already runs pilot %s\n\n", version)
	}
	return nil
}

// agentAdapter lets `pilot doctor` inspect and repair agents without the doctor
// package knowing how an agent is obtained.
type agentAdapter struct{ app *app.App }

func (ad agentAdapter) Status(ctx context.Context, host string) doctor.AgentReport {
	rc, state, cause := ad.app.InspectAgent(ctx, host)
	rep := doctor.AgentReport{Expected: proto.Version}
	if cause != nil {
		rep.Detail = firstLine(cause.Error())
	}

	switch state {
	case app.AgentReady:
		info, err := rc.Check(ctx)
		if err != nil {
			rep.Unreachable = true
			return rep
		}
		rep.Installed, rep.Build, rep.Protocol = true, info.Build, info.Protocol
	case app.AgentSkewed:
		rep.Installed, rep.Skewed = true, true
		var skew *remote.SkewError
		if errors.As(cause, &skew) {
			rep.Protocol = skew.Agent
		}
	case app.AgentAbsent:
		// Installed stays false.
	default:
		rep.Unreachable = true
	}
	return rep
}

func (ad agentAdapter) Upgrade(ctx context.Context, host string) error {
	_, err := ad.app.SyncAgent(ctx, host, app.AgentSync{
		Version:   version,
		ModuleDir: moduleDir(),
	})
	return err
}

func newAgentStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report each host's agent version and whether it matches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentStatus(cmd.Context(), g)
		},
	}
}

func runAgentStatus(ctx context.Context, g *globals) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	type agentRow struct {
		Host     string `json:"host"`
		State    string `json:"state"`
		Build    string `json:"build,omitempty"`
		Protocol int    `json:"protocol,omitempty"`
		Expected int    `json:"expected_protocol"`
		Detail   string `json:"detail,omitempty"`
	}
	var rows []agentRow
	emit := func(r agentRow) { rows = append(rows, r) }

	if !g.json {
		fmt.Printf("\n  pilot %s\n\n", version)
	}

	var stale bool
	for _, host := range a.Fleet.HostNames() {
		rc, state, cause := a.InspectAgent(ctx, host)
		switch state {
		case app.AgentReady:
			info, _ := rc.Check(ctx)

			// Protocol-compatible but a different build still deserves a nudge:
			// it is running code this CLI did not ship, and saying nothing here
			// would contradict `agent upgrade`, which does replace it.
			if info.Build != version {
				stale = true
				emit(agentRow{Host: host, State: "build-drift", Build: info.Build,
					Protocol: info.Protocol, Expected: proto.Version,
					Detail: fmt.Sprintf("agent %s, this pilot is %s", info.Build, version)})
				if !g.json {
					fmt.Printf("  ⚠ %-12s agent %s, this pilot is %s (protocol %d, compatible)\n",
						host, info.Build, version, info.Protocol)
				}
				continue
			}
			emit(agentRow{Host: host, State: "ready", Build: info.Build,
				Protocol: info.Protocol, Expected: proto.Version})
			if !g.json {
				fmt.Printf("  ✔ %-12s agent %s (protocol %d)\n", host, info.Build, info.Protocol)
			}
		case app.AgentSkewed:
			stale = true
			emit(agentRow{Host: host, State: "skewed", Expected: proto.Version,
				Detail: firstLine(cause.Error())})
			if !g.json {
				fmt.Printf("  ⚠ %-12s %s\n", host, firstLine(cause.Error()))
			}
		case app.AgentAbsent:
			stale = true
			emit(agentRow{Host: host, State: "absent", Expected: proto.Version})
			if !g.json {
				fmt.Printf("  – %-12s no agent installed\n", host)
			}
		default:
			emit(agentRow{Host: host, State: "unreachable", Expected: proto.Version,
				Detail: firstLine(cause.Error())})
			if !g.json {
				fmt.Printf("  ✖ %-12s %s\n", host, firstLine(cause.Error()))
			}
		}
	}

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return err
		}
		if stale {
			return exitWith(2)
		}
		return nil
	}

	fmt.Println()
	if stale {
		fmt.Println("  bring them up to date with `pilot agent upgrade`")
		fmt.Println()
		return exitWith(2)
	}
	return nil
}
