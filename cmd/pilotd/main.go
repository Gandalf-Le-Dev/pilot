// Command pilotd is Pilot's per-host agent.
//
// It has two modes. `pilotd serve` is the daemon systemd runs: it owns
// activation, health verification, automatic rollback, and drift detection.
// `pilotd ctl` is the thin client the CLI invokes over SSH, which talks to that
// daemon through a Unix socket.
//
// The split matters. Because a job runs in the daemon rather than in the ctl
// process, an SSH connection dropping mid-deploy kills only the observer — the
// deploy still completes, or still rolls back.
//
// `pilotd server` is a third, separate process: the status page, on one host.
// It ships in this binary so the server host needs nothing installed beyond
// what every host has, but it runs as its own unprivileged unit and shares no
// state with the agent. The agent itself still listens on nothing but its
// socket.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent"
	"github.com/Gandalf-Le-Dev/pilot/internal/agent/client"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/server"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// Injected at build time by GoReleaser. See cmd/pilot for why the version
// matters beyond display.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := &cobra.Command{
		Use:           "pilotd",
		Short:         "Pilot's per-host agent",
		Version:       fmt.Sprintf("%s (%s, protocol %d)", version, commit, proto.Version),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd(), newCtlCmd(), newServerCmd(os.Geteuid))

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "pilotd: %v\n", err)
		os.Exit(1)
	}
}

func newServeCmd() *cobra.Command {
	var socket, root, host, caddyfile, snippets, admin string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the agent daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := agent.New(agent.Options{
				Root: root, Host: host, Build: version,
				Caddyfile: caddyfile, Snippets: snippets, Admin: admin,
			})
			if err != nil {
				return err
			}

			// Background loops start before the socket does, so the first
			// status query already has something to report.
			a.StartLoops(cmd.Context())
			return a.Serve(cmd.Context(), socket)
		},
	}

	cmd.Flags().StringVar(&socket, "socket", proto.DefaultSocket, "unix socket to listen on")
	cmd.Flags().StringVar(&root, "root", release.DefaultRoot, "Pilot's directory on this host")
	cmd.Flags().StringVar(&host, "host", "", "name for this host (default: hostname)")
	cmd.Flags().StringVar(&caddyfile, "caddyfile", "", "path to the global Caddyfile")
	cmd.Flags().StringVar(&snippets, "snippet-dir", "", "directory Pilot owns for generated routes")
	cmd.Flags().StringVar(&admin, "caddy-admin", "", "Caddy admin API base URL")

	return cmd
}

// newServerCmd takes the euid as a function so a test can show the root
// check runs before anything is read: the configuration holds notifier
// credentials, and root should be turned away before it touches them.
func newServerCmd(euid func() int) *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Serve the public status page and take reports from agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := server.RefuseRoot(euid()); err != nil {
				return err
			}
			if configPath == "" {
				dir := os.Getenv("CREDENTIALS_DIRECTORY")
				if dir == "" {
					return fmt.Errorf("no --config, and no $CREDENTIALS_DIRECTORY from systemd's LoadCredential=")
				}
				configPath = filepath.Join(dir, server.CredentialName)
			}
			cfg, err := server.LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("%s: %w", configPath, err)
			}
			return server.Run(cmd.Context(), cfg)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "",
		"configuration file (default: "+server.CredentialName+" in $CREDENTIALS_DIRECTORY)")
	return cmd
}

// newCtlCmd is the client the CLI invokes over SSH. Every verb prints JSON on
// stdout, because its only consumer is another program.
func newCtlCmd() *cobra.Command {
	var socket string

	cmd := &cobra.Command{
		Use:   "ctl",
		Short: "Query the local agent (machine-readable)",
	}
	cmd.PersistentFlags().StringVar(&socket, "socket", proto.DefaultSocket, "agent socket")

	emit := func(v any) error {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "info",
		Short: "Report the agent's protocol version and build",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			out, err := client.NewUnix(socket).Info(c.Context())
			if err != nil {
				return err
			}
			return emit(out)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Report every service this agent manages",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			out, err := client.NewUnix(socket).Status(c.Context())
			if err != nil {
				return err
			}
			return emit(out)
		},
	})

	cmd.AddCommand(func() *cobra.Command {
		var since int64
		c := &cobra.Command{
			Use:   "dashboard",
			Short: "Report status, resource samples, alert episodes, and deploy history in one response",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				out, err := client.NewUnix(socket).Dashboard(c.Context(), since)
				if err != nil {
					return err
				}
				return emit(out)
			},
		}
		c.Flags().Int64Var(&since, "since", 0, "only samples newer than this unix time")
		return c
	}())

	cmd.AddCommand(&cobra.Command{
		Use:   "drift",
		Short: "Report configuration that no longer matches its manifest",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			out, err := client.NewUnix(socket).Drift(c.Context())
			if err != nil {
				return err
			}
			return emit(out)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "alerts",
		Short: "Report which alert rules are currently firing",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			out, err := client.NewUnix(socket).Alerts(c.Context())
			if err != nil {
				return err
			}
			return emit(out)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "config",
		Short: "Install host-wide notifiers and alert rules (request on stdin)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			var req proto.ConfigRequest
			if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
				return fmt.Errorf("reading request: %w", err)
			}
			return client.NewUnix(socket).PutConfig(c.Context(), req.Spec)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "put-service <name>",
		Short: "Cache a service definition without deploying (spec on stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			spec, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
			if err != nil {
				return fmt.Errorf("reading spec: %w", err)
			}
			return client.NewUnix(socket).PutService(c.Context(), args[0], string(spec))
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "deploy",
		Short: "Activate a staged release (request on stdin)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			var req proto.DeployRequest
			if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
				return fmt.Errorf("reading request: %w", err)
			}
			out, err := client.NewUnix(socket).Deploy(c.Context(), req)
			if err != nil {
				return err
			}
			return emit(out)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "rollback",
		Short: "Return a service to an earlier release (request on stdin)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			var req proto.RollbackRequest
			if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
				return fmt.Errorf("reading request: %w", err)
			}
			out, err := client.NewUnix(socket).Rollback(c.Context(), req)
			if err != nil {
				return err
			}
			return emit(out)
		},
	})

	var wait bool
	var after int
	jobCmd := &cobra.Command{
		Use:   "job <id>",
		Short: "Report a job's progress",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl := client.NewUnix(socket)
			var (
				out *proto.Job
				err error
			)
			if wait {
				out, err = cl.WaitJob(c.Context(), args[0], after)
			} else {
				out, err = cl.Job(c.Context(), args[0])
			}
			if err != nil {
				return err
			}
			return emit(out)
		},
	}
	jobCmd.Flags().BoolVar(&wait, "wait", false, "block until the job changes or finishes")
	jobCmd.Flags().IntVar(&after, "after", 0, "only report events beyond this index")
	cmd.AddCommand(jobCmd)

	return cmd
}
