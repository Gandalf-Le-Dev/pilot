package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/registry"
	"github.com/Gandalf-Le-Dev/pilot/internal/render"
	"github.com/Gandalf-Le-Dev/pilot/internal/updates"
)

func newUpdatesCmd(g *globals) *cobra.Command {
	var failOnUpdate bool

	cmd := &cobra.Command{
		Use:   "updates",
		Short: "Report newer versions of the images the fleet runs",
		Long: "Reads the image references your compose files declare, asks each registry\n" +
			"what tags exist, and reports what is newer.\n\n" +
			"Nothing is pulled, nothing is deployed, and no host is contacted — this is a\n" +
			"read against public registry metadata, so it works with no fleet access at\n" +
			"all.\n\n" +
			"A tag naming a series rather than a release — `postgres:17`, `redis:8` — is\n" +
			"listed but not compared. Those are chosen precisely so they move, and\n" +
			"announcing the next major every day is how a checker becomes something you\n" +
			"stop reading.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdates(cmd.Context(), g, failOnUpdate)
		},
	}

	cmd.Flags().BoolVar(&failOnUpdate, "exit-code", false,
		"exit 1 when an update is available, for use in a scheduled check")
	return cmd
}

func runUpdates(ctx context.Context, g *globals, failOnUpdate bool) error {
	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	// Deliberately not RequireValid: a fleet with a configuration error still
	// runs images that may have security updates, and refusing to say so until
	// an unrelated field is fixed helps nobody.
	rows := updates.Check(ctx, a.Fleet, registry.New())

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return err
		}
	} else {
		fmt.Println()
		render.Updates(os.Stdout, rows)
		fmt.Println()
	}

	if failOnUpdate {
		for _, r := range rows {
			if r.Outdated() {
				return exitWith(1)
			}
		}
	}
	return nil
}
