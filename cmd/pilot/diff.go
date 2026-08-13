package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/remote"
	"github.com/Gandalf-Le-Dev/pilot/internal/app"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/render"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
)

func newDiffCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "diff [service|@tag]",
		Short: "Show where the live configuration no longer matches what was deployed",
		Long: "Compares each service's live state against the release that is supposed to be\n" +
			"running: the runtime's configuration fingerprint, and the installed Caddy route\n" +
			"against the copy that shipped with the release.\n\n" +
			"This is the check that catches a hand-edited compose file or route — the kind\n" +
			"that works fine until the next deploy silently reverts it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			return runDiff(cmd.Context(), g, selector)
		},
	}
}

func runDiff(ctx context.Context, g *globals, selector string) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	services, err := selectServices(a, selector)
	if err != nil {
		return err
	}

	// One agent lookup per host, reused across that host's services.
	agents := map[string]*remote.Client{}
	drifts := map[string]map[string]*driftInfo{}
	for _, host := range hostsFor(services) {
		rc, _ := a.AgentOrNil(ctx, host)
		agents[host] = rc
		if rc != nil {
			if d, err := rc.Drift(ctx); err == nil {
				m := map[string]*driftInfo{}
				for name, v := range d {
					m[name] = &driftInfo{config: v.Config, route: v.Route, release: v.Release, detail: v.Detail}
				}
				drifts[host] = m
			}
		}
	}

	var entries []render.DriftEntry
	for _, s := range services {
		for _, host := range s.Hosts {
			entries = append(entries, diffOne(ctx, a, s, host, agents[host], drifts[host]))
		}
	}

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	fmt.Println()
	render.WriteDrift(os.Stdout, entries)
	fmt.Println()

	for _, e := range entries {
		if e.Drifted() {
			return exitWith(2)
		}
	}
	return nil
}

type driftInfo struct {
	config  bool
	route   bool
	release string
	detail  string
}

// diffOne determines one service's drift on one host.
//
// The agent's answer is preferred when there is one — it has been watching, so
// its verdict reflects a fingerprint taken while the service was quiet rather
// than one taken right now. Without an agent, Pilot computes the same
// comparison over SSH, which costs a few round-trips but works.
func diffOne(ctx context.Context, a *app.App, s *config.Service, host string, rc *remote.Client, known map[string]*driftInfo) render.DriftEntry {
	e := render.DriftEntry{Service: s.Name, Host: host}

	t, err := a.Target(s, host, "")
	if err != nil {
		e.Unavailable = err.Error()
		return e
	}

	current, err := runtime.ReadCurrent(ctx, t)
	if err != nil {
		e.Unavailable = "cannot reach " + host + ": " + firstLine(err.Error())
		return e
	}
	if current == "" {
		e.Unavailable = "not deployed on this host"
		return e
	}
	e.Release = current

	if rc != nil {
		if d, ok := known[s.Name]; ok {
			e.ConfigDrift, e.RouteDrift, e.Detail = d.config, d.route, d.detail
		}
	} else {
		// Degraded mode: no agent, so compute config drift here.
		e.ConfigDrift = computeConfigDrift(ctx, a, s, t, current)
	}

	// Route drift is cheap to inspect either way, and it is the one with
	// something worth showing line by line.
	if s.Expose != nil {
		before, after, differs := routeTexts(ctx, a, t, s.Name, current)
		if rc == nil {
			e.RouteDrift = differs
		}
		if e.RouteDrift {
			e.RouteBefore, e.RouteAfter = before, after
		}
	}
	return e
}

// computeConfigDrift compares the live fingerprint against the baseline the
// agent recorded at deploy time.
func computeConfigDrift(ctx context.Context, a *app.App, s *config.Service, t *runtime.Target, current string) bool {
	rt, err := app.RuntimeFor(s)
	if err != nil {
		return false
	}
	live, err := rt.Fingerprint(ctx, t)
	if err != nil {
		return false
	}
	recorded, err := t.Client.ReadFile(ctx, path.Join(a.Layout.Release(s.Name, current), release.FingerprintFile))
	if err != nil {
		return false // no baseline yet; nothing to compare against
	}
	return string(recorded) != "" && string(recorded) != live
}

// routeTexts returns the route that shipped with the release and the one
// actually installed.
func routeTexts(ctx context.Context, a *app.App, t *runtime.Target, service, current string) (before, after string, differs bool) {
	deployed, err := t.Client.ReadFile(ctx, a.Layout.Snippet(service, current))
	if err != nil {
		return "", "", false
	}
	installed, err := t.Client.ReadFile(ctx, path.Join(a.Fleet.Caddy.SnippetDir, caddy.SnippetName(service)))
	if err != nil {
		// The release carries a route but none is installed, which is itself
		// a divergence worth reporting.
		return string(deployed), "", true
	}
	return string(deployed), string(installed), string(deployed) != string(installed)
}

func hostsFor(services []*config.Service) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range services {
		for _, h := range s.Hosts {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}
