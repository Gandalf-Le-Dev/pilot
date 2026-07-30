package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Gandalf-Le-Dev/pilot/internal/skill"
)

// SkillInstallDir is where `--install` writes, relative to the fleet directory.
const SkillInstallDir = ".claude/skills/pilot"

func newSkillCmd(g *globals) *cobra.Command {
	var install bool

	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Print the agent skill, or install it for Claude Code",
		Long: "Pilot ships instructions for an AI agent operating a fleet: the concepts that\n" +
			"are not guessable from the command names, and the handful of flags an agent\n" +
			"must never use.\n\n" +
			"It is advisory. Pilot does not enforce it and does not detect whether an agent\n" +
			"is calling — a deploy is announced through your notifiers whoever ran it, and a\n" +
			"failed one rolls itself back either way.\n\n" +
			"Printed to stdout by default, so it can be piped anywhere. `--install` writes\n" +
			"it to " + SkillInstallDir + " for Claude Code to pick up.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSkill(g, install)
		},
	}
	cmd.Flags().BoolVar(&install, "install", false,
		"write the skill to "+SkillInstallDir+" instead of printing it")
	return cmd
}

func runSkill(g *globals, install bool) error {
	body := []byte(skill.Content())

	if !install {
		_, err := os.Stdout.Write(body)
		return err
	}

	var err error

	// Alongside the fleet configuration, since that is the directory an agent
	// will be working in.
	root := g.dir
	if root == "" {
		root, err = os.Getwd()
		if err != nil {
			return err
		}
	}

	dir := filepath.Join(root, SkillInstallDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(target, body, 0o644); err != nil {
		return err
	}

	fmt.Printf("\n  wrote %s\n", target)
	fmt.Printf("  it is advisory — Pilot enforces nothing in it, and a deploy is\n")
	fmt.Printf("  announced through your notifiers however it was started\n\n")
	return nil
}
