package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/doctor"
	"github.com/Gandalf-Le-Dev/pilot/internal/render"
)

func newDoctorCmd(g *globals) *cobra.Command {
	var offline, fix bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that the fleet configuration, hosts, and routing are sound",
		Long: "Runs every check Pilot knows: configuration validity, host reachability and\n" +
			"prerequisites, the Caddy import line and generated routes, disk headroom, and\n" +
			"DNS and TLS for each exposed domain.\n\n" +
			"Exit codes: 0 clean, 1 errors present, 2 warnings only — so it drops straight\n" +
			"into cron or CI.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.Context(), g, offline, fix)
		},
	}

	cmd.Flags().BoolVar(&offline, "offline", false,
		"skip every check that needs the network, validating configuration only")
	cmd.Flags().BoolVar(&fix, "fix", false,
		"apply the unambiguously safe repairs; never touches a running service")

	return cmd
}

func runDoctor(ctx context.Context, g *globals, offline, fix bool) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	env := &doctor.Env{Fleet: a.Fleet, Diags: a.Diags, Offline: offline}
	if !offline {
		env.Clients = a.ConnectAll(ctx, a.Fleet.HostNames())
	}

	report := doctor.Run(ctx, env, doctor.Standard())

	if fix {
		if err := applyFixes(ctx, report, offline); err != nil {
			return err
		}
		// Re-run so the report reflects reality after the repairs, rather than
		// showing problems that no longer exist.
		if !offline {
			env.Clients = a.ConnectAll(ctx, a.Fleet.HostNames())
		}
		report = doctor.Run(ctx, env, doctor.Standard())
	}

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Println()
		render.Doctor(os.Stdout, report, a.Fleet.HostNames())
		fmt.Println()
	}

	if code := report.ExitCode(); code != 0 {
		return exitWith(code)
	}
	return nil
}

// applyFixes runs the repairs attached to findings, reporting each one.
//
// Failures are collected rather than fatal: one host refusing a repair should
// not stop the others from being fixed.
func applyFixes(ctx context.Context, report *doctor.Report, offline bool) error {
	fixes := report.Fixable()
	if len(fixes) == 0 {
		return nil
	}
	if offline {
		fmt.Fprintln(os.Stderr, "pilot: --fix needs the network; drop --offline to apply repairs")
		return nil
	}

	var failed int
	for _, f := range fixes {
		label := f.Title
		if f.Host != "" {
			label = f.Host + ": " + label
		}
		fmt.Printf("  fixing %s — %s\n", label, f.FixDesc)

		if err := f.Fix(ctx); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "    failed: %v\n", err)
			continue
		}
		fmt.Println("    done")
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d repair(s) failed\n", failed)
	}
	return nil
}
