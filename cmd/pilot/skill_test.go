package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSkillInstallDir(t *testing.T) {
	// Fleet-local: alongside the given fleet directory.
	got, err := skillInstallDir("/fleet", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/fleet", SkillInstallDir) {
		t.Errorf("local install dir = %q", got)
	}

	// Global: under the home directory, and the fleet directory must not
	// leak into the path — `-g` means "every project", not this one.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err = skillInstallDir("/fleet", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, SkillGlobalDir) {
		t.Errorf("global install dir = %q, want it under %s", got, home)
	}
}
