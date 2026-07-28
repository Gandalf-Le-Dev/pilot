package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/goccy/go-yaml"

	"github.com/gandalfledev/pilot/internal/agent"
	"github.com/gandalfledev/pilot/internal/agent/install"
	"github.com/gandalfledev/pilot/internal/agent/remote"
	"github.com/gandalfledev/pilot/internal/app"
	"github.com/gandalfledev/pilot/internal/edge/caddy"
	"github.com/gandalfledev/pilot/internal/transport/proto"
	"github.com/gandalfledev/pilot/internal/transport/ssh"
)

func newBootstrapCmd(g *globals) *cobra.Command {
	var opts bootstrapOpts

	cmd := &cobra.Command{
		Use:   "bootstrap [host|@tag]",
		Short: "Prepare a host for Pilot and install its agent",
		Long: "Creates Pilot's directory, checks the host's prerequisites, wires the global\n" +
			"Caddyfile to import Pilot's generated routes, and installs the pilotd agent\n" +
			"under systemd.\n\n" +
			"The agent is what makes a deploy safe: it owns health verification and the\n" +
			"automatic rollback that depends on it, so closing your laptop mid-deploy\n" +
			"cannot leave a service activated with nobody to back it out.\n\n" +
			"Idempotent. Re-run it to upgrade an agent after updating Pilot.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.selector = args[0]
			}
			return runBootstrap(cmd.Context(), g, opts)
		},
	}

	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "skip the confirmation prompt before editing the Caddyfile")
	cmd.Flags().BoolVar(&opts.skipAgent, "no-agent", false, "prepare the host but do not install the agent")
	cmd.Flags().StringVar(&opts.agentBinary, "agent-binary", os.Getenv("PILOT_AGENT_BIN"),
		"path to a prebuilt pilotd binary (default: found alongside pilot, or built from source)")

	return cmd
}

type bootstrapOpts struct {
	selector    string
	yes         bool
	skipAgent   bool
	agentBinary string
}

func runBootstrap(ctx context.Context, g *globals, opts bootstrapOpts) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.RequireValid(); err != nil {
		return err
	}

	hosts, err := selectHosts(a, opts.selector)
	if err != nil {
		return err
	}

	var failed int
	for _, name := range hosts {
		fmt.Printf("\n  %s\n", name)
		if err := bootstrapHost(ctx, a, name, opts); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "    ✖ %v\n", err)
		}
	}

	fmt.Println()
	if failed > 0 {
		return exitWith(1)
	}
	fmt.Println("  bootstrap complete — run `pilot doctor` to verify")
	fmt.Println()
	return nil
}

// selectHosts resolves a host name, an @tag, or an empty selector meaning
// every host in the fleet.
func selectHosts(a *app.App, selector string) ([]string, error) {
	all := a.Fleet.HostNames()
	switch {
	case selector == "":
		return all, nil
	case strings.HasPrefix(selector, "@"):
		tag := strings.TrimPrefix(selector, "@")
		var out []string
		for _, name := range all {
			if a.Fleet.Hosts[name].HasTag(tag) {
				out = append(out, name)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no hosts are tagged %q", tag)
		}
		return out, nil
	}
	if _, ok := a.Fleet.Hosts[selector]; !ok {
		return nil, fmt.Errorf("no such host %q", selector)
	}
	return []string{selector}, nil
}

func bootstrapHost(ctx context.Context, a *app.App, host string, opts bootstrapOpts) error {
	client, err := a.Client(host)
	if err != nil {
		return err
	}

	if res, err := client.Run(ctx, "true"); err != nil {
		return fmt.Errorf("unreachable: %w", err)
	} else if !res.OK() {
		return fmt.Errorf("ssh works but commands fail: %w", res.Err())
	}
	fmt.Println("    ✔ reachable")

	// Create the service root. Nothing else Pilot does works without it, and
	// creating it here means the first deploy has one less thing to fail on.
	layout := a.Layout
	if err := client.MkdirAll(ctx, layout.Services()); err != nil {
		return fmt.Errorf("creating %s: %w", layout.Services(), err)
	}
	fmt.Printf("    ✔ %s ready\n", layout.Services())

	if err := reportPrereqs(ctx, a, client, host); err != nil {
		return err
	}

	if hostNeedsCaddy(a, host) {
		if err := ensureCaddyImport(ctx, a, client, opts.yes); err != nil {
			return err
		}
	}

	if opts.skipAgent {
		fmt.Println("    – agent installation skipped (--no-agent)")
		return nil
	}
	return installAgent(ctx, a, client, host, opts)
}

// installAgent puts pilotd on the host and starts it under systemd.
func installAgent(ctx context.Context, a *app.App, client *ssh.Client, host string, opts bootstrapOpts) error {
	arch, err := install.DetectArch(ctx, client)
	if err != nil {
		return err
	}

	src := install.Source{Explicit: opts.agentBinary, ModuleDir: moduleDir()}
	binary, origin, cleanup, err := src.Resolve(ctx, arch)
	if err != nil {
		return err
	}
	defer cleanup()
	fmt.Printf("    ✔ agent for linux/%s (%s)\n", arch, origin)

	if _, err := install.Install(ctx, client, binary, install.Options{
		Host: host, Layout: a.Layout, Caddy: a.CaddyPaths(),
	}); err != nil {
		return err
	}

	// Confirm the daemon actually came up and speaks our protocol. Reporting
	// success on the strength of `systemctl restart` returning 0 would be a
	// lie: the unit can start and the process exit a second later.
	rc := remote.New(client, host, a.Layout)
	info, err := waitForAgent(ctx, rc)
	if err != nil {
		return fmt.Errorf("the agent was installed but did not come up: %w\n"+
			"      check it with:  ssh %s journalctl -u pilotd -n 50 --no-pager", err, host)
	}

	fmt.Printf("    ✔ agent running (build %s, protocol %d)\n", info.Build, info.Protocol)

	// Push the host-wide half of the configuration. Without it the agent can
	// evaluate rules but has nowhere to send them, and alerting silently does
	// nothing — which is the worst possible failure mode for alerting.
	spec, err := fleetConfigSpec(a)
	if err != nil {
		return err
	}
	if err := rc.PutConfig(ctx, spec); err != nil {
		return fmt.Errorf("installing alert configuration: %w", err)
	}
	if n := len(a.Fleet.Notifiers); n > 0 {
		fmt.Printf("    ✔ %d notifier(s) and %d host rule(s) installed\n", n, len(a.Fleet.Alerts))
	}
	return nil
}

// fleetConfigSpec renders the host-wide configuration the agent needs to alert
// on its own: notifiers and host-scoped rules.
func fleetConfigSpec(a *app.App) (string, error) {
	body, err := yaml.Marshal(agent.FleetConfig{
		Notifiers: a.Fleet.Notifiers,
		Alerts:    a.Fleet.Alerts,
	})
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// waitForAgent polls until the daemon answers, since systemd returns as soon as
// it has forked rather than when the process is ready.
func waitForAgent(ctx context.Context, rc *remote.Client) (*proto.Info, error) {
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
		case <-time.After(time.Second):
		}
	}
	return nil, last
}

// moduleDir returns the Pilot checkout to build the agent from, when the CLI is
// being run from inside its own repository.
func moduleDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "cmd", "pilotd")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// reportPrereqs checks only what this host actually needs — a box serving
// static sites has no reason to have Docker.
func reportPrereqs(ctx context.Context, a *app.App, client *ssh.Client, host string) error {
	var needsDocker bool
	for _, s := range a.Fleet.Services {
		for _, h := range s.Hosts {
			if h == host && s.Runtime == "compose" {
				needsDocker = true
			}
		}
	}

	if needsDocker {
		if ok, _ := client.HasCommand(ctx, "docker"); !ok {
			return fmt.Errorf("docker is not installed, but compose services are placed on this host")
		}
		if res, _ := client.Run(ctx, "docker info"); !res.OK() {
			return fmt.Errorf("the ssh user cannot reach the docker daemon; add it to the docker group")
		}
		fmt.Println("    ✔ docker available")
	}

	if hostNeedsCaddy(a, host) {
		if ok, _ := client.HasCommand(ctx, "caddy"); !ok {
			return fmt.Errorf("caddy is not installed, but exposed services are placed on this host")
		}
		fmt.Println("    ✔ caddy available")
	}
	return nil
}

func hostNeedsCaddy(a *app.App, host string) bool {
	for _, s := range a.Fleet.Services {
		if s.Expose != nil && slices.Contains(s.Hosts, host) {
			return true
		}
	}
	return false
}

// ensureCaddyImport adds Pilot's import line, showing the operator exactly what
// will change before it changes.
func ensureCaddyImport(ctx context.Context, a *app.App, client *ssh.Client, yes bool) error {
	paths := a.CaddyPaths()

	raw, err := client.ReadFile(ctx, paths.Caddyfile)
	if err != nil {
		return fmt.Errorf("reading %s: %w\nPilot needs a global Caddyfile to import its routes from", paths.Caddyfile, err)
	}

	state, reason := caddy.Inspect(string(raw), paths.Caddyfile, paths.SnippetDir)
	switch state {
	case caddy.ImportPresent:
		fmt.Printf("    ✔ %s already imports Pilot's routes\n", paths.Caddyfile)
		return nil
	case caddy.ImportUnsafe:
		return fmt.Errorf("%s\n      %s", reason, caddy.UnsafeHint)
	}

	directive := caddy.ImportDirective(paths.Caddyfile, paths.SnippetDir)
	if !yes {
		fmt.Printf("\n    Pilot will append this to %s:\n\n", paths.Caddyfile)
		fmt.Printf("        # added by pilot — loads one generated route per service\n")
		fmt.Printf("        %s\n\n", directive)
		fmt.Printf("    A backup is taken first, and restored automatically if caddy\n")
		fmt.Printf("    rejects the result. Nothing else in the file is touched.\n\n")

		ok, err := confirm("    Proceed? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf("    skipped — add this line yourself to finish setup:\n        %s\n", directive)
			return nil
		}
	}

	action, err := caddy.EnsureImport(ctx, client, paths, time.Now())
	if err != nil {
		return err
	}
	if action == caddy.ActionNone {
		fmt.Printf("    ✔ %s already imports Pilot's routes\n", paths.Caddyfile)
		return nil
	}
	fmt.Printf("    ✔ %s now imports %s\n", paths.Caddyfile, paths.SnippetDir)
	fmt.Printf("      backup: %s\n", caddy.BackupName(paths.Caddyfile, time.Now()))
	return nil
}

func confirm(prompt string) (bool, error) {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, nil // treat EOF as declining, never as consent
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
