package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gandalfledev/pilot/internal/app"
	"github.com/gandalfledev/pilot/internal/deploy"
	"github.com/gandalfledev/pilot/internal/edge/caddy"
	"github.com/gandalfledev/pilot/internal/runtime"
)

func newRollbackCmd(g *globals) *cobra.Command {
	var to string

	cmd := &cobra.Command{
		Use:   "rollback <service>",
		Short: "Return a service to an earlier release",
		Long: "Activates the previous release and restores the route that shipped with it,\n" +
			"so code and routing move back together.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRollback(cmd.Context(), g, args[0], to)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "release to activate (default: the previous one)")
	return cmd
}

func runRollback(ctx context.Context, g *globals, name, to string) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	s, ok := a.Fleet.Services[name]
	if !ok {
		return fmt.Errorf("no such service %q", name)
	}
	if !s.Deployable() {
		return fmt.Errorf("service %q is `manage: observe`; Pilot does not manage its releases", name)
	}

	rt, err := app.RuntimeFor(s)
	if err != nil {
		return err
	}

	log := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	fmt.Println()

	var failed int
	for _, host := range s.Hosts {
		t, err := a.Target(s, host, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✖ %s: %v\n", host, err)
			failed++
			continue
		}
		// Prefer the agent: a rollback is a deploy in reverse, and it deserves
		// the same guarantee that closing this terminal cannot strand it.
		if rc, _ := a.AgentOrNil(ctx, host); rc != nil {
			if err := deploy.RollbackViaAgent(ctx, rc, name, to, deploy.Deployer(), log); err != nil {
				fmt.Fprintf(os.Stderr, "  ✖ %s: %v\n", host, err)
				failed++
				continue
			}
		} else if err := deploy.Rollback(ctx, rt, t, a.CaddyPaths(), to, log); err != nil {
			fmt.Fprintf(os.Stderr, "  ✖ %s: %v\n", host, err)
			failed++
			continue
		}
		fmt.Printf("  ✔ %s rolled back\n", host)
	}

	fmt.Println()
	if failed > 0 {
		return exitWith(1)
	}
	return nil
}

func newReleasesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "releases <service>",
		Short: "Show a service's release history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReleases(cmd.Context(), g, args[0])
		},
	}
}

func runReleases(ctx context.Context, g *globals, name string) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	s, ok := a.Fleet.Services[name]
	if !ok {
		return fmt.Errorf("no such service %q", name)
	}

	for _, host := range s.Hosts {
		t, err := a.Target(s, host, "")
		if err != nil {
			return err
		}
		st, err := runtime.ReadState(ctx, t)
		if err != nil {
			return err
		}
		if _, err := runtime.Reconcile(ctx, t, st); err != nil {
			return err
		}

		fmt.Printf("\n  %s on %s\n", name, host)
		if len(st.History) == 0 {
			fmt.Println("    no deploys recorded")
			continue
		}
		for _, r := range st.History {
			marker := " "
			if r.Release == st.Current {
				marker = "*"
			}
			fmt.Printf("    %s %s  %-12s %s  %s\n",
				marker, r.Release, r.Outcome,
				r.StartedAt.Local().Format("2006-01-02 15:04"), r.By)
			if r.Reason != "" {
				fmt.Printf("        %s\n", r.Reason)
			}
		}
	}
	fmt.Println()
	return nil
}

func newLogsCmd(g *globals) *cobra.Command {
	var (
		follow bool
		tail   int
		since  string
	)

	cmd := &cobra.Command{
		Use:   "logs <service>",
		Short: "Stream a service's logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd.Context(), g, args[0], runtime.LogOptions{
				Follow: follow, Tail: tail, Since: since,
			})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new output")
	cmd.Flags().IntVar(&tail, "tail", 200, "number of lines to show from the end")
	cmd.Flags().StringVar(&since, "since", "", "show logs since a timestamp or duration, e.g. 15m")
	return cmd
}

func runLogs(ctx context.Context, g *globals, name string, opts runtime.LogOptions) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	s, ok := a.Fleet.Services[name]
	if !ok {
		return fmt.Errorf("no such service %q", name)
	}
	rt, err := app.RuntimeFor(s)
	if err != nil {
		return err
	}

	// Multi-host follow would interleave streams without saying which host
	// each line came from; until the output is prefixed, ask for one host.
	if len(s.Hosts) > 1 && opts.Follow {
		return fmt.Errorf("%s runs on %d hosts; following them all at once would interleave\n"+
			"unlabelled output — narrow it down for now", name, len(s.Hosts))
	}

	for _, host := range s.Hosts {
		t, err := a.Target(s, host, "")
		if err != nil {
			return err
		}
		if len(s.Hosts) > 1 {
			fmt.Printf("\n==> %s on %s <==\n", name, host)
		}
		if err := rt.Logs(ctx, t, opts, os.Stdout); err != nil {
			if ctx.Err() != nil {
				return nil // interrupted by the operator, not a failure
			}
			return err
		}
	}
	return nil
}

func newRoutesCmd(g *globals) *cobra.Command {
	var prune bool

	cmd := &cobra.Command{
		Use:   "routes",
		Short: "Show the Caddy routes Pilot manages",
		Long: "Lists the generated route files on each host and flags any with no matching\n" +
			"service — usually left behind when a service was removed from config.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRoutes(cmd.Context(), g, prune)
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false, "remove orphaned routes and reload caddy")
	return cmd
}

type routeRow struct {
	Host    string   `json:"host"`
	Service string   `json:"service"`
	Domains []string `json:"domains,omitempty"`
	Orphan  bool     `json:"orphan,omitempty"`
	Pruned  bool     `json:"pruned,omitempty"`
}

func runRoutes(ctx context.Context, g *globals, prune bool) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	paths := a.CaddyPaths()
	var rows []routeRow

	for _, host := range a.Fleet.HostNames() {
		client, err := a.Client(host)
		if err != nil {
			continue
		}
		installed, err := caddy.ListSnippets(ctx, client, paths)
		if err != nil || len(installed) == 0 {
			continue
		}

		known := map[string]bool{}
		for _, n := range a.Fleet.ServiceNames() {
			s := a.Fleet.Services[n]
			if s.Expose != nil && slices.Contains(s.Hosts, host) {
				known[n] = true
			}
		}

		for _, svc := range installed {
			row := routeRow{Host: host, Service: svc, Orphan: !known[svc]}
			if s, ok := a.Fleet.Services[svc]; ok && s.Expose != nil {
				row.Domains = s.Expose.Domains
			}
			if row.Orphan && prune {
				if _, err := caddy.RemoveSnippet(ctx, client, paths, svc); err != nil {
					fmt.Fprintf(os.Stderr, "  ✖ %s: removing %s: %v\n", host, svc, err)
				} else {
					row.Pruned = true
				}
			}
			rows = append(rows, row)
		}
	}

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	fmt.Println()
	if len(rows) == 0 {
		fmt.Println("  no routes are installed")
		fmt.Println()
		return nil
	}

	orphans := 0
	for _, r := range rows {
		switch {
		case r.Pruned:
			fmt.Printf("  ✔ %s  %s  pruned\n", r.Host, r.Service)
		case r.Orphan:
			orphans++
			fmt.Printf("  ⚠ %s  %s  orphaned (no such service on this host)\n", r.Host, r.Service)
		default:
			fmt.Printf("  ✔ %s  %s  %s\n", r.Host, r.Service, strings.Join(r.Domains, ", "))
		}
	}
	if orphans > 0 && !prune {
		fmt.Printf("\n  %d orphaned route(s) — remove with `pilot routes --prune`\n", orphans)
	}
	fmt.Println()

	if orphans > 0 && !prune {
		return exitWith(2)
	}
	return nil
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return head
}
