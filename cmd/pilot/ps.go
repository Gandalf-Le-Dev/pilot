package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/app"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/render"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
)

func newPsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "ps [service|@tag]",
		Short: "Show the containers and processes behind each service",
		Long: "Where `status` answers \"is it up\", this answers \"what exactly is up\": one\n" +
			"line per container or process, with its image, health, and age.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			return runPs(cmd.Context(), g, selector)
		},
	}
}

func runPs(ctx context.Context, g *globals, selector string) error {
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

	var rows []render.InstanceRow
	for _, s := range services {
		for _, host := range s.Hosts {
			obs, why := observationFor(ctx, a, s, host, views)
			if why != "" {
				// A host we could not ask about is not a host with nothing on
				// it. Dropping the row would quietly imply the second.
				rows = append(rows, render.InstanceRow{
					Service: s.Name, Host: host, Name: "?",
					State: "unknown", Detail: why,
				})
				continue
			}
			if len(obs.Instances) == 0 {
				rows = append(rows, render.InstanceRow{
					Service: s.Name, Host: host, Name: "–",
					State: string(obs.State), Detail: obs.Detail,
				})
				continue
			}
			for _, in := range obs.Instances {
				rows = append(rows, render.InstanceRow{
					Service: s.Name, Host: host, Name: in.Name,
					State: in.State, Health: in.Health, Image: in.Image,
					Age: age(in.Since), Detail: in.Detail,
				})
			}
		}
	}

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	fmt.Println()
	render.Instances(os.Stdout, rows)
	fmt.Println()
	return nil
}

// observationFor prefers the agent's answer and falls back to observing the
// host directly. The second return explains why nothing could be observed,
// empty when the observation succeeded.
func observationFor(ctx context.Context, a *app.App, s *config.Service, host string, views hostViews) (runtime.Observation, string) {
	if hv := views[host]; hv != nil {
		if got, ok := hv.services[s.Name]; ok {
			return got.Obs, ""
		}
	}

	rt, err := app.RuntimeFor(s)
	if err != nil {
		return runtime.Observation{}, firstLine(err.Error())
	}
	t, err := a.Target(s, host, "")
	if err != nil {
		return runtime.Observation{}, firstLine(err.Error())
	}
	obs, err := rt.Observe(ctx, t)
	if err != nil {
		return runtime.Observation{}, firstLine(err.Error())
	}
	return obs, ""
}

// age renders how long something has been up, coarsely — the precision that is
// actually useful when scanning a list.
func age(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	d := time.Since(since)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func newTopCmd(g *globals) *cobra.Command {
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "top [service|@tag]",
		Short: "Watch the fleet, refreshing until interrupted",
		Long: "A repeating `status`. Useful while a deploy lands, or when waiting to see\n" +
			"whether something settles.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			return runTop(cmd.Context(), g, selector, interval)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 3*time.Second, "how often to refresh")
	return cmd
}

func runTop(ctx context.Context, g *globals, selector string, interval time.Duration) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	services, err := selectServices(a, selector)
	if err != nil {
		return err
	}
	if interval < time.Second {
		interval = time.Second
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		views := gatherHosts(ctx, a, hostsFor(services))
		rows := buildRows(ctx, a, services, views)

		var b strings.Builder
		// Clear and home, so the table redraws in place rather than scrolling.
		b.WriteString("\033[H\033[2J")
		fmt.Fprintf(&b, "  pilot — %s (refreshing every %s, ctrl-c to stop)\n\n",
			time.Now().Format("15:04:05"), interval)
		render.Status(&b, rows)
		render.StatusFooter(&b, views.degraded(), views.alerts())
		fmt.Print(b.String())

		select {
		case <-ctx.Done():
			// Leave the terminal on a fresh line rather than mid-frame.
			fmt.Println()
			return nil
		case <-tick.C:
		}
	}
}
