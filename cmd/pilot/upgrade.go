package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/install"
	"github.com/Gandalf-Le-Dev/pilot/internal/selfupdate"
)

func newUpgradeCmd(g *globals) *cobra.Command {
	var checkOnly bool

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update pilot to the latest release",
		Long: "Checks for a newer release and replaces this binary with it, after verifying\n" +
			"the download against the release checksums and confirming it runs.\n\n" +
			"Upgrading the CLI does not upgrade the agents already on your hosts. Because\n" +
			"an agent is fetched from the release of the CLI that installed it, a newer CLI\n" +
			"will refuse to talk to an older agent rather than guess. Run `pilot agent\n" +
			"upgrade` afterwards to bring them up to match — or just deploy, which repairs\n" +
			"a skewed agent rather than quietly proceeding without one.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd.Context(), checkOnly)
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false, "report whether an update exists, without installing it")
	return cmd
}

func runUpgrade(ctx context.Context, checkOnly bool) error {
	fmt.Println()

	rel, err := selfupdate.Latest(ctx)
	if err != nil {
		return err
	}

	// A source build has no version to compare against, so an "upgrade" would
	// silently replace a binary that is probably newer than the release.
	if !install.IsReleaseVersion(version) {
		fmt.Printf("  this is a %s build, not a release\n", version)
		fmt.Printf("  latest release is %s — install it with `brew upgrade pilot`,\n", rel.TagName)
		fmt.Printf("  or rebuild from source\n\n")
		return exitWith(2)
	}

	switch {
	case !selfupdate.Newer(version, rel.Version()):
		fmt.Printf("  pilot %s is the latest release\n\n", version)
		return nil
	default:
		fmt.Printf("  %s → %s\n", version, rel.Version())
		fmt.Printf("  %s\n\n", rel.HTMLURL)
	}

	if checkOnly {
		return exitWith(2) // an update is available; useful in a cron check
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	// Refuse to fight a package manager. Overwriting a Homebrew-managed binary
	// leaves brew describing a file that is no longer what it thinks, and the
	// next `brew upgrade` undoes the change.
	if selfupdate.Detect(exe) == selfupdate.MethodHomebrew {
		fmt.Printf("  pilot was installed with Homebrew, which owns this binary.\n")
		fmt.Printf("  upgrade it with:\n\n      brew upgrade pilot\n\n")
		return exitWith(2)
	}

	log := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	if err := selfupdate.Apply(ctx, rel, exe, log); err != nil {
		return err
	}

	fmt.Printf("\n  upgraded to %s\n", rel.Version())
	fmt.Printf("  agents on your hosts still run their original version:\n\n")
	fmt.Printf("      pilot agent upgrade\n\n")
	fmt.Printf("  a deploy repairs a skewed agent on its own, so this is a convenience\n")
	fmt.Printf("  rather than something you must remember\n\n")
	return nil
}
