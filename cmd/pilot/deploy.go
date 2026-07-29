package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/remote"
	"github.com/Gandalf-Le-Dev/pilot/internal/app"
	"github.com/Gandalf-Le-Dev/pilot/internal/build"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/deploy"
	"github.com/Gandalf-Le-Dev/pilot/internal/render"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/secrets"
)

func newDeployCmd(g *globals) *cobra.Command {
	var (
		planOnly       bool
		ref            string
		noVerify       bool
		force          bool
		noAgentUpgrade bool
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
				noAgentUpgrade: noAgentUpgrade,
			})
		},
	}

	cmd.Flags().BoolVar(&planOnly, "plan", false, "show what would change, then stop")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref to deploy, overriding the service's configured ref")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false,
		"skip health verification (also disables the automatic rollback that depends on it)")
	cmd.Flags().BoolVar(&force, "force", false, "deploy a `manage: observe` service anyway")
	cmd.Flags().BoolVar(&noAgentUpgrade, "no-agent-upgrade", false,
		"do not repair a version-skewed agent; deploy without one instead (loses automatic rollback)")

	return cmd
}

type deployOpts struct {
	planOnly bool
	ref      string
	noVerify bool
	force    bool

	// noAgentUpgrade opts out of repairing a skewed agent, accepting a deploy
	// with no automatic rollback rather than reinstalling the daemon.
	noAgentUpgrade bool
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

	log := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }

	// Find the agents up front, so the plan can say which hosts will have the
	// deploy owned by a daemon and which will not.
	//
	// A version-skewed agent is repaired here rather than counted as absent.
	//
	// Falling back to driving the host over SSH looks like graceful degradation
	// and is not one: it silently gives up the automatic rollback that is the
	// agent's whole reason to exist, and it does so because of the unrelated act
	// of upgrading the CLI. Reinstalling is exactly what `pilot agent upgrade`
	// would do, from the same version-locked, checksum-verified source, and this
	// deploy is already about to change the host.
	agents := map[string]*remote.Client{}

	// Carry *why* a host has no usable agent, not just that it hasn't one —
	// otherwise the warning below cannot name the right remedy.
	type degraded struct {
		host   string
		skewed bool
	}
	var agentless []degraded

	for _, host := range s.Hosts {
		rc, state, cause := a.InspectAgent(ctx, host)

		if state == app.AgentSkewed && !opts.noAgentUpgrade {
			log("  %s: %s", host, firstLine(cause.Error()))
			log("  %s: upgrading the agent before deploying", host)

			if _, err := a.SyncAgent(ctx, host, app.AgentSync{
				Version:   version,
				ModuleDir: moduleDir(),
				Log:       func(f string, args ...any) { log("    ✔ "+f, args...) },
			}); err != nil {
				return fmt.Errorf("the agent on %s is too old for this pilot and could not be upgraded: %w\n\n"+
					"deploying anyway would silently lose automatic rollback — fix the agent, or pass\n"+
					"--no-agent-upgrade to accept that", host, err)
			}
			rc, state, _ = a.InspectAgent(ctx, host)
		}

		if state == app.AgentReady {
			agents[host] = rc
		} else {
			agentless = append(agentless, degraded{host: host, skewed: state == app.AgentSkewed})
		}
	}

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
		// Name the right remedy. A skewed agent is present and running, so
		// "no agent … pilot bootstrap" sends someone to install what is
		// already there; the fix is to upgrade it, or to stop passing
		// --no-agent-upgrade.
		hosts := make([]string, 0, len(agentless))
		for _, h := range agentless {
			hosts = append(hosts, h.host)
		}
		fmt.Printf("  ! no usable agent on %s — the deploy will run from here, so losing\n",
			strings.Join(hosts, ", "))
		fmt.Printf("    this terminal mid-deploy leaves nothing to finish the rollback.\n")

		if agentless[0].skewed {
			fmt.Printf("    fix with: pilot agent upgrade %s\n\n", agentless[0].host)
		} else {
			fmt.Printf("    fix with: pilot bootstrap %s\n\n", agentless[0].host)
		}
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
