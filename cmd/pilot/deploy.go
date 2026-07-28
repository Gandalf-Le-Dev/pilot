package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gandalfledev/pilot/internal/agent/remote"
	"github.com/gandalfledev/pilot/internal/app"
	"github.com/gandalfledev/pilot/internal/build"
	"github.com/gandalfledev/pilot/internal/config"
	"github.com/gandalfledev/pilot/internal/deploy"
	"github.com/gandalfledev/pilot/internal/render"
	"github.com/gandalfledev/pilot/internal/runtime"
	"github.com/gandalfledev/pilot/internal/secrets"
)

func newDeployCmd(g *globals) *cobra.Command {
	var (
		planOnly bool
		ref      string
		noVerify bool
		force    bool
	)

	cmd := &cobra.Command{
		Use:   "deploy <service|@tag>",
		Short: "Build, ship, and activate a new release",
		Long: "Runs the pipeline: resolve → build (locally) → plan → stage → activate → verify.\n\n" +
			"Everything that can fail is made to fail during staging, while the running\n" +
			"service is still untouched. If the health check fails after activation, the\n" +
			"previous release and its route are restored automatically.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeploy(cmd.Context(), g, args[0], deployOpts{
				planOnly: planOnly, ref: ref, noVerify: noVerify, force: force,
			})
		},
	}

	cmd.Flags().BoolVar(&planOnly, "plan", false, "show what would change, then stop")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref to deploy, overriding the service's configured ref")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false,
		"skip health verification (also disables the automatic rollback that depends on it)")
	cmd.Flags().BoolVar(&force, "force", false, "deploy a `manage: observe` service anyway")

	return cmd
}

type deployOpts struct {
	planOnly bool
	ref      string
	noVerify bool
	force    bool
}

func runDeploy(ctx context.Context, g *globals, selector string, opts deployOpts) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.RequireValid(); err != nil {
		return err
	}

	services, err := a.Fleet.ResolveTargets(selector)
	if err != nil {
		return err
	}

	// An observe-only service is skipped silently in a fleet-wide deploy — that
	// is the whole point of the flag — but naming one directly deserves an
	// explanation rather than silence.
	var deployable []*config.Service
	for _, s := range services {
		switch {
		case s.Deployable():
			deployable = append(deployable, s)
		case opts.force && len(services) == 1:
			deployable = append(deployable, s)
		case len(services) == 1:
			return fmt.Errorf("service %q is `manage: observe`, so Pilot never deploys it\n"+
				"this guard exists so a fleet-wide deploy cannot recreate your database\n"+
				"if you really mean to, run: pilot deploy %s --force", s.Name, s.Name)
		default:
			fmt.Printf("  skipping %s (manage: observe)\n", s.Name)
		}
	}
	if len(deployable) == 0 {
		return fmt.Errorf("nothing to deploy")
	}

	for _, s := range deployable {
		if err := deployService(ctx, a, s, opts, g.json); err != nil {
			return err
		}
	}
	return nil
}

func deployService(ctx context.Context, a *app.App, s *config.Service, opts deployOpts, asJSON bool) error {
	if opts.ref != "" && s.Source != nil {
		clone := *s.Source
		clone.Ref = opts.ref
		s.Source = &clone
	}

	rt, err := app.RuntimeFor(s)
	if err != nil {
		return err
	}

	targets := map[string]*runtime.Target{}
	for _, host := range s.Hosts {
		t, err := a.Target(s, host, "")
		if err != nil {
			return err
		}
		targets[host] = t
	}

	// Find the agents up front, so the plan can say which hosts will have the
	// deploy owned by a daemon and which will not.
	agents := map[string]*remote.Client{}
	var agentless []string
	for _, host := range s.Hosts {
		if rc, _ := a.AgentOrNil(ctx, host); rc != nil {
			agents[host] = rc
		} else {
			agentless = append(agentless, host)
		}
	}

	log := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }

	// ---- build, on this machine; never on a target host.
	src, err := build.Resolve(ctx, s, a.Root, sourceCache())
	if err != nil {
		return err
	}
	result, err := build.Build(ctx, s, src, log)
	if err != nil {
		return err
	}
	defer result.Cleanup()

	env, err := resolveEnv(s)
	if err != nil {
		return err
	}

	plan, err := deploy.Compute(ctx, deploy.Input{
		Service: s, Fleet: a.Fleet, Layout: a.Layout,
		Build: result, Env: env, Targets: targets,
	})
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(plan); err != nil {
			return err
		}
	} else {
		fmt.Println()
		render.Plan(os.Stdout, plan)
		fmt.Println()
	}

	if len(agentless) > 0 && !asJSON {
		fmt.Printf("  ! no agent on %s — the deploy will run from here, so losing this\n",
			strings.Join(agentless, ", "))
		fmt.Printf("    terminal mid-deploy leaves nothing to finish the rollback.\n")
		fmt.Printf("    fix with: pilot bootstrap %s\n\n", agentless[0])
	}

	if opts.planOnly {
		return nil
	}
	if plan.NoOp() {
		fmt.Println("  nothing to do — every host already runs this release")
		fmt.Println()
		return nil
	}

	spec, err := deploy.SpecFor(s)
	if err != nil {
		return err
	}

	exec := &deploy.Executor{
		Runtime: rt, Targets: targets, Agents: agents, Caddy: a.CaddyPaths(),
		Env: env, Log: log, SkipVerify: opts.noVerify,
		Spec: spec, By: deploy.Deployer(),
	}

	outcomes, runErr := exec.Run(ctx, plan)
	fmt.Println()
	render.Outcomes(os.Stdout, outcomes)
	fmt.Println()

	if runErr != nil {
		return exitWith(1)
	}
	return nil
}

// resolveEnv expands secret references in the service's environment.
//
// A reference Pilot cannot resolve is an error rather than a passthrough:
// shipping the literal "${sops:...}" as a password would deploy cleanly and
// fail somewhere far from the cause.
func resolveEnv(s *config.Service) (map[string]string, error) {
	return secrets.ResolveMap(s.Env)
}

func sourceCache() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pilot-src")
	}
	return filepath.Join(home, ".pilot", "src")
}
