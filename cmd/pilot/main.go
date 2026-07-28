// Command pilot is the control plane for a small fleet of servers.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/gandalfledev/pilot/internal/app"
)

// version is overridden at build time with -ldflags.
var version = "dev"

// exitError carries a specific process exit status. Commands return it when
// the status is meaningful to a script, not just "something failed".
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

// exitWith returns an error that sets the process exit code without printing
// anything extra — used where the command has already written its own report.
func exitWith(code int) error { return &exitError{code: code} }

// globals are the flags every command shares.
type globals struct {
	dir  string
	json bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g := &globals{}
	root := &cobra.Command{
		Use:   "pilot",
		Short: "Monitor, deploy, and update the services on your servers",
		Long: "Pilot deploys and watches the services on a small fleet of servers:\n" +
			"docker compose stacks, systemd units, and static sites, with Caddy as\n" +
			"the front door.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVarP(&g.dir, "dir", "C", "",
		"directory holding fleet.yaml (default: search upward from the current directory)")
	root.PersistentFlags().BoolVar(&g.json, "json", false, "emit machine-readable JSON")

	root.AddCommand(
		newDoctorCmd(g),
		newBootstrapCmd(g),
		newDeployCmd(g),
		newRollbackCmd(g),
		newStatusCmd(g),
		newDiffCmd(g),
		newPsCmd(g),
		newTopCmd(g),
		newReleasesCmd(g),
		newLogsCmd(g),
		newRoutesCmd(g),
	)

	if err := root.ExecuteContext(ctx); err != nil {
		if ee, ok := errors.AsType[*exitError](err); ok {
			if ee.err != nil {
				fmt.Fprintf(os.Stderr, "pilot: %v\n", ee.err)
			}
			os.Exit(ee.code)
		}
		fmt.Fprintf(os.Stderr, "pilot: %v\n", err)
		os.Exit(1)
	}
}

// load opens the fleet configuration for a command.
func (g *globals) load() (*app.App, error) {
	a, err := app.Load(g.dir)
	if err != nil {
		return nil, err
	}
	a.JSON = g.json
	return a, nil
}
