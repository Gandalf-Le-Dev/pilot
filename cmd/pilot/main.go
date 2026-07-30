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

	"github.com/Gandalf-Le-Dev/pilot/internal/app"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// Injected at build time by GoReleaser:
//
//	-ldflags "-X main.version=... -X main.commit=... -X main.date=..."
//
// The version is not cosmetic. `pilot bootstrap` uses it to fetch the pilotd
// asset from its *own* release, which is what makes agent/CLI protocol skew
// impossible rather than merely unlikely.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

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

// jsonAnnotation marks how a command treats the global --json flag.
//
// Every command carries one, and a test walks the tree to make sure of it. The
// flag is global, so a command that quietly ignores it leaves a caller waiting
// for output that never comes — and "somebody will remember to wire it up" is
// exactly the assumption that put `manage` and `runtime` outside `status --json`
// in the first place. Forgetting now fails the build instead.
const jsonAnnotation = "json"

// How a command may answer --json.
const (
	jsonStructured = "structured" // a single JSON document
	jsonNDJSON     = "ndjson"     // one object per line, for streams that never end
	jsonRefused    = "refused"    // errors, pointing at a command that does support it
	jsonNone       = "none"       // interactive or progress-only; nothing to serialise
	jsonGroup      = "group"      // a parent command; its children carry the annotation
)

// annotate tags a command with its --json behaviour.
func annotate(cmd *cobra.Command, how string) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[jsonAnnotation] = how
	return cmd
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := buildRoot(&globals{}).ExecuteContext(ctx); err != nil {
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

// buildRoot assembles the command tree.
//
// Separate from main so a test can walk it. Every command is wrapped in
// annotate(), which is what makes the --json guard possible.
func buildRoot(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "pilot",
		Short: "Monitor, deploy, and update the services on your servers",
		Long: "Pilot deploys and watches the services on a small fleet of servers:\n" +
			"docker compose stacks, systemd units, and static sites, with Caddy as\n" +
			"the front door.",
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVarP(&g.dir, "dir", "C", "",
		"directory holding fleet.yaml (default: search upward from the current directory)")
	root.PersistentFlags().BoolVar(&g.json, "json", false, "emit machine-readable JSON")

	root.AddCommand(
		annotate(newInitCmd(g), jsonNone),
		annotate(newUpgradeCmd(g), jsonNone),
		annotate(newDoctorCmd(g), jsonStructured),
		annotate(newBootstrapCmd(g), jsonNone),
		annotate(newAgentCmd(g), jsonGroup),
		annotate(newDeployCmd(g), jsonStructured),
		annotate(newRollbackCmd(g), jsonStructured),
		annotate(newStatusCmd(g), jsonStructured),
		annotate(newDiffCmd(g), jsonStructured),
		annotate(newPsCmd(g), jsonStructured),
		annotate(newTopCmd(g), jsonRefused),
		annotate(newReleasesCmd(g), jsonStructured),
		annotate(newLogsCmd(g), jsonNDJSON),
		annotate(newRoutesCmd(g), jsonStructured),
		annotate(newSkillCmd(g), jsonNone),
	)
	return root
}

// versionString renders the build identity. Commit and date come from
// GoReleaser; a `go build` shows the placeholders, which is itself useful
// information when someone reports a bug.
func versionString() string {
	if commit == "none" {
		return version
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return fmt.Sprintf("%s (%s, %s, protocol %d)", version, commit, date, proto.Version)
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
