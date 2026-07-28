package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

func newInitCmd(g *globals) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init [directory]",
		Short: "Create a fleet configuration to start from",
		Long: "Writes a fleet.yaml and an example service, both commented, in a directory\n" +
			"that becomes the source of truth for what runs on your servers.\n\n" +
			"Keep it in its own git repository, separate from any application code: it\n" +
			"describes your infrastructure, and every deploy reads it rather than the\n" +
			"other way round.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return runInit(cmd.Context(), dir, force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing fleet.yaml")
	return cmd
}

func runInit(ctx context.Context, dir string, force bool) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	fleetPath := filepath.Join(abs, config.FleetFile)
	if _, err := os.Stat(fleetPath); err == nil && !force {
		return fmt.Errorf("%s already exists\npass --force to overwrite it", fleetPath)
	}

	// Refuse to scaffold inside an existing fleet, which would leave two
	// configurations disagreeing about the same hosts.
	if parent, err := discoverParent(abs); err == nil && parent != abs {
		return fmt.Errorf("%s already belongs to the fleet rooted at %s\n"+
			"a nested fleet would mean two files disagreeing about the same hosts", abs, parent)
	}

	if err := os.MkdirAll(filepath.Join(abs, config.ServicesDir), 0o755); err != nil {
		return err
	}

	written := []string{}
	write := func(rel, body string) error {
		path := filepath.Join(abs, rel)
		if _, err := os.Stat(path); err == nil && !force {
			return nil // never clobber an existing service definition
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	}

	if err := write(config.FleetFile, fleetTemplate); err != nil {
		return err
	}
	if err := write(filepath.Join(config.ServicesDir, "example.yaml"), serviceTemplate); err != nil {
		return err
	}
	if err := write(".gitignore", gitignoreTemplate); err != nil {
		return err
	}

	fmt.Printf("\n  created in %s\n", abs)
	for _, f := range written {
		fmt.Printf("    %s\n", f)
	}

	fmt.Print(`
  next:
    1. edit fleet.yaml — set your host's address, and `)
	fmt.Print("`sudo: true` unless you\n       connect as root\n")
	fmt.Println("    2. rename services/example.yaml and describe a real service")
	fmt.Println("    3. pilot doctor        — check the config, the host, DNS and TLS")
	fmt.Println("    4. pilot bootstrap <host>")
	fmt.Println("    5. pilot deploy <service> --plan")
	fmt.Println()
	fmt.Println("  keep this directory in its own git repository")
	fmt.Println()
	return nil
}

// discoverParent reports the fleet root above dir, if any.
func discoverParent(dir string) (string, error) {
	for cur := filepath.Dir(dir); ; {
		if _, err := os.Stat(filepath.Join(cur, config.FleetFile)); err == nil {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", os.ErrNotExist
		}
		cur = parent
	}
}

const fleetTemplate = `version: 1

defaults:
  # Releases kept per service. Rollback needs at least two.
  keep_releases: 5

hosts:
  # The name is yours; it is what you type in ` + "`hosts:`" + ` and ` + "`pilot deploy @tag`" + `.
  web-1:
    # A hostname, an IP, or a Host alias from ~/.ssh/config. Pilot shells out
    # to the system ssh, so aliases, ProxyJump and hardware keys all work with
    # nothing repeated here.
    address: web1.example.com

    # Pilot writes to /opt/pilot and /etc/caddy and drives systemd. Unless you
    # connect as root, it needs passwordless sudo — a deploy has no terminal to
    # answer a password prompt.
    sudo: true

    # Selectors: ` + "`pilot deploy @prod`" + ` acts on every service on a tagged host.
    tags: [prod]

caddy:
  caddyfile:   /etc/caddy/Caddyfile
  snippet_dir: /etc/caddy/pilot.d
  admin:       http://127.0.0.1:2019

# Alert delivery. Each is a single POST or exec — no retries, no templating.
# Anything needing either should be a ` + "`command`" + ` pointing at a program that
# does it properly, which is also how email is handled.
#
# notifiers:
#   phone: {type: ntfy, url: "https://ntfy.sh/my-fleet-alerts"}
#
# Host-wide rules. Per-service rules live on the service.
#
# alerts:
#   - when: host.disk.free_pct < 10
#     notify: [phone]
`

const serviceTemplate = `# Rename this file to match the service. One file per service.

name: example
runtime: compose        # compose | static | systemd
hosts: [web-1]

# Where the code comes from. Use ` + "`path:`" + ` for something in this repository,
# or ` + "`repo:`" + ` to clone it. Builds always run on your machine, never on the host.
source:
  path: src/example

# What to ship. For a compose service whose only artifact is the compose file,
# ` + "`output: [\"./\"]`" + ` sends the source directory's contents.
build:
  output: ["./"]

compose:
  file: compose.yaml

# Values are referenced, never stored here:
#   ${env:NAME}   the shell, or a gitignored .env beside fleet.yaml
#   ${cmd:...}    any command — a keychain, pass, Vault, 1Password
#   ${file:path}  a file on disk
#
# env:
#   LOG_LEVEL: info
#   API_TOKEN: ${cmd:security find-generic-password -s pilot/example -a API_TOKEN -w}

# The front door. Pilot generates a Caddy site block from this.
expose:
  domains: [example.com]
  upstream: 8080        # the host port Caddy proxies to
  verify: true          # also check the public URL after deploying

# Without one, a broken deploy cannot be detected or rolled back automatically.
health:
  http:
    url: http://127.0.0.1:8080/healthz
  timeout: 60s

rollout:
  # recreate   — replace the containers; a brief blip, held by Caddy retry
  # blue-green — start the new version beside the old and flip; needs
  #              ` + "`service:`" + ` and two ` + "`ports:`" + `, and costs 2x memory while deploying
  strategy: recreate

# alerts:
#   - when: service.down
#     for: 2m
#     notify: [phone]
`

const gitignoreTemplate = `# Secrets are referenced, never stored. Nothing here should hold one, but
# Pilot also refuses to read a .env that is not chmod 600.
.env
*.key
*.pem
`
