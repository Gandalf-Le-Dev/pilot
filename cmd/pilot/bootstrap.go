package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/app"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/ssh"
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
	return cmd
}

type bootstrapOpts struct {
	selector  string
	yes       bool
	skipAgent bool
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
	return installAgent(ctx, a, host)
}

// installAgent puts pilotd on the host and starts it under systemd.
//
// The work lives in app.SyncAgent, shared with `pilot agent upgrade` and with
// the deploy path that repairs a skewed agent — three callers that must install
// the same binary the same way.
func installAgent(ctx context.Context, a *app.App, host string) error {
	_, err := a.SyncAgent(ctx, host, app.AgentSync{
		Version:   version,
		ModuleDir: moduleDir(),
		Log:       func(f string, args ...any) { fmt.Printf("    ✔ "+f+"\n", args...) },
	})
	return err
}

// moduleDir finds a Pilot checkout to build the agent from.
//
// It searches upward from the working directory and from the binary's own
// location. The second is not redundant: a development build is normally run
// against a fleet repository somewhere else entirely, and searching only the
// working directory loses the checkout the binary came from — leaving a build
// that can prove nothing about the agent it installs, and so must refuse to
// install one.
func moduleDir() string {
	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if self, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		starts = append(starts, filepath.Dir(self))
	}

	for _, start := range starts {
		if dir := findModuleRoot(start); dir != "" {
			return dir
		}
	}
	return ""
}

// findModuleRoot walks up from dir looking for Pilot's own module.
//
// Both checks matter: go.mod alone would match any Go project the fleet
// repository happens to sit inside, and building ./cmd/pilotd there would fail
// in a way that looks like Pilot's fault.
func findModuleRoot(start string) string {
	for dir := start; ; {
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
