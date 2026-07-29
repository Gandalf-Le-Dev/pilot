package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindModuleRootRequiresPilotItself(t *testing.T) {
	// A fleet repository is often its own Go module, and it sits nowhere near
	// Pilot's source. Matching on go.mod alone would pick it, then fail to
	// build ./cmd/pilotd in a way that reads like Pilot's bug.
	fleet := t.TempDir()
	if err := os.WriteFile(filepath.Join(fleet, "go.mod"), []byte("module fleet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findModuleRoot(fleet); got != "" {
		t.Errorf("findModuleRoot = %q, want none: that module has no cmd/pilotd", got)
	}

	pilot := t.TempDir()
	if err := os.WriteFile(filepath.Join(pilot, "go.mod"), []byte("module pilot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pilot, "cmd", "pilotd"), 0o755); err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join(pilot, "internal", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findModuleRoot(nested); got != pilot {
		t.Errorf("findModuleRoot(%q) = %q, want %q", nested, got, pilot)
	}
}
