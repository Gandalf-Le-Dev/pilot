package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/dashboard"
)

func newDashboardCmd(g *globals) *cobra.Command {
	var (
		port   int
		noOpen bool
	)

	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Serve a read-only web view of the fleet",
		Long: "A local web page showing the whole fleet at once: what is running, each\n" +
			"service's CPU and memory as a share of its host, alert history, and recent\n" +
			"deploys.\n\n" +
			"It runs on this machine and dies with it. Hosts are reached over the same\n" +
			"SSH channels every other command uses — nothing new listens anywhere, and\n" +
			"the page is served on 127.0.0.1 only. It is read-only: deploying and rolling\n" +
			"back stay in the CLI, where intent is typed rather than clicked.\n\n" +
			"Resource history is whatever each agent holds in memory (about six hours);\n" +
			"an agent restart starts it afresh. Anyone needing weeks of retention needs\n" +
			"Prometheus, not a deploy tool.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDashboard(cmd.Context(), g, port, noOpen)
		},
	}
	cmd.Flags().IntVar(&port, "port", 5480, "port to serve on (loopback only)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the URL instead of opening the browser")
	return cmd
}

func runDashboard(ctx context.Context, g *globals, port int, noOpen bool) error {
	// The dashboard is HTML by construction; its data form is the sum of
	// `status --json`, `doctor --json`, and `releases --json`.
	if g.json {
		return fmt.Errorf("`dashboard` serves a web page and has no JSON form\n" +
			"poll `pilot status --json` instead, or the other --json commands it aggregates")
	}

	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	manage := map[string]string{}
	for name, s := range a.Fleet.Services {
		manage[name] = string(s.Manage)
	}

	srv := dashboard.New(dashboard.Source{
		Hosts:  a.Fleet.HostNames(),
		Manage: manage,
		Connect: func(ctx context.Context, host string) (dashboard.HostClient, string) {
			rc, errMsg := a.AgentOrNil(ctx, host)
			if rc == nil {
				return nil, errMsg
			}
			return rc, ""
		},
	})

	url, err := srv.Serve(ctx, port)
	if err != nil {
		return err
	}

	fmt.Printf("\n  dashboard on %s — ctrl-c to stop\n\n", url)
	if !noOpen {
		openBrowser(url)
	}

	// Run blocks polling the fleet until the context ends (ctrl-c).
	srv.Run(ctx)
	return nil
}

// openBrowser is best-effort: a URL that has to be clicked is not a failure.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// Give the listener a beat, then fire and forget.
	time.AfterFunc(200*time.Millisecond, func() { _ = cmd.Start() })
}
