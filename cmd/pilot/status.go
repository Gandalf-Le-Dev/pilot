package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/app"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/render"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

func newStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status [service|@tag]",
		Short: "Show what is running across the fleet",
		Long: "Asks each host's agent, which answers for every service it manages in one\n" +
			"round-trip and adds what only it knows: configuration drift and which alert\n" +
			"rules are firing.\n\n" +
			"Hosts without an agent are still reported — Pilot observes them directly over\n" +
			"SSH — but drift and alerts are unavailable there, and the output says so.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			return runStatus(cmd.Context(), g, selector)
		},
	}
}

func runStatus(ctx context.Context, g *globals, selector string) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	services, err := selectServices(a, selector)
	if err != nil {
		return err
	}

	views := gatherHosts(ctx, a, hostsFor(services))
	rows := buildRows(ctx, a, services, views)

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	fmt.Println()
	render.Status(os.Stdout, rows)
	render.StatusFooter(os.Stdout, views.degraded(), views.alerts())
	fmt.Println()

	// Exit 2 means "reachable but not right", so cron can tell that apart from
	// Pilot itself failing.
	for _, r := range rows {
		if runtime.State(r.State) != runtime.StateRunning || r.Drift != "" {
			return exitWith(2)
		}
	}
	return nil
}

// selectServices expands a selector, defaulting to the whole fleet.
func selectServices(a *app.App, selector string) ([]*config.Service, error) {
	if selector == "" {
		var out []*config.Service
		for _, n := range a.Fleet.ServiceNames() {
			out = append(out, a.Fleet.Services[n])
		}
		return out, nil
	}
	return a.Fleet.ResolveTargets(selector)
}

// hostView is what one host's agent reported.
type hostView struct {
	host     string
	services map[string]proto.ServiceStatus
	firing   []string
	disk     *proto.DiskUsage

	// degraded explains why the agent could not be used, empty when it could.
	degraded string
}

type hostViews map[string]*hostView

func (v hostViews) degraded() map[string]string {
	out := map[string]string{}
	for host, hv := range v {
		if hv.degraded != "" {
			out[host] = hv.degraded
		}
	}
	return out
}

func (v hostViews) alerts() map[string][]string {
	out := map[string][]string{}
	for host, hv := range v {
		if len(hv.firing) > 0 {
			out[host] = hv.firing
		}
	}
	return out
}

// gatherHosts queries every host's agent concurrently.
//
// One call per host rather than one per service: the agent already knows about
// everything it manages, so a fleet-wide status costs a handful of round-trips
// instead of one per (service, host) pair.
func gatherHosts(ctx context.Context, a *app.App, hosts []string) hostViews {
	views := hostViews{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			hv := &hostView{host: host, services: map[string]proto.ServiceStatus{}}

			rc, why := a.AgentOrNil(ctx, host)
			if rc == nil {
				hv.degraded = why
			} else {
				if st, err := rc.Status(ctx); err == nil {
					for _, s := range st.Services {
						hv.services[s.Name] = s
					}
					hv.disk = st.Disk
				} else {
					hv.degraded = err.Error()
				}
				if al, err := rc.Alerts(ctx); err == nil {
					hv.firing = al.Firing
				}
			}

			mu.Lock()
			views[host] = hv
			mu.Unlock()
		}(host)
	}
	wg.Wait()
	return views
}

// buildRows merges agent answers with direct observation for anything the agent
// does not know about — a service configured but never deployed, or a host with
// no agent at all.
func buildRows(ctx context.Context, a *app.App, services []*config.Service, views hostViews) []render.StatusRow {
	type key struct {
		svc  *config.Service
		host string
	}
	var pairs []key
	for _, s := range services {
		for _, h := range s.Hosts {
			pairs = append(pairs, key{s, h})
		}
	}

	rows := make([]render.StatusRow, len(pairs))
	var wg sync.WaitGroup

	for i, p := range pairs {
		hv := views[p.host]
		if hv != nil {
			if got, ok := hv.services[p.svc.Name]; ok {
				rows[i] = rowFromAgent(p.svc, p.host, got)
				continue
			}
		}
		wg.Add(1)
		go func(i int, s *config.Service, host string) {
			defer wg.Done()
			rows[i] = observeDirect(ctx, a, s, host)
		}(i, p.svc, p.host)
	}
	wg.Wait()
	return rows
}

func rowFromAgent(s *config.Service, host string, got proto.ServiceStatus) render.StatusRow {
	row := render.StatusRow{
		Service: s.Name,
		Host:    host,
		State:   string(got.Obs.State),
		Release: got.Obs.Release,
		Detail:  got.Obs.Detail,
	}
	if got.Error != "" {
		row.State = string(runtime.StateUnknown)
		row.Detail = got.Error
	}
	if got.Drift.Any() {
		switch {
		case got.Drift.Config && got.Drift.Route:
			row.Drift = "config+route"
		case got.Drift.Config:
			row.Drift = "config"
		default:
			row.Drift = "route"
		}
	}
	if !s.Deployable() {
		row.Detail = appendDetail(row.Detail, "observe only")
	}
	return row
}

// observeDirect is the degraded path: no agent, so Pilot drives the host itself.
func observeDirect(ctx context.Context, a *app.App, s *config.Service, host string) render.StatusRow {
	row := render.StatusRow{Service: s.Name, Host: host, State: string(runtime.StateUnknown)}

	rt, err := app.RuntimeFor(s)
	if err != nil {
		row.Detail = err.Error()
		return row
	}
	t, err := a.Target(s, host, "")
	if err != nil {
		row.Detail = err.Error()
		return row
	}

	obs, err := rt.Observe(ctx, t)
	if err != nil {
		row.Detail = firstLine(err.Error())
		return row
	}

	row.State = string(obs.State)
	row.Release = obs.Release
	row.Detail = obs.Detail
	if row.Detail == "" && len(obs.Instances) > 0 {
		row.Detail = fmt.Sprintf("%d instance(s)", len(obs.Instances))
	}
	if !s.Deployable() {
		row.Detail = appendDetail(row.Detail, "observe only")
	}
	return row
}

func appendDetail(detail, extra string) string {
	if detail == "" {
		return extra
	}
	return detail + " · " + extra
}
