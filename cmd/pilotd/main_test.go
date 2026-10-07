package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func quiet(c *cobra.Command) *cobra.Command {
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	return c
}

func TestServerRefusesRootBeforeReadingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	cmd := quiet(newServerCmd(func() int { return 0 }))
	cmd.SetArgs([]string{"--config", missing})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "refusing to run as root") {
		t.Fatalf("err = %v, want the root refusal before the missing file is noticed", err)
	}

	cmd = quiet(newServerCmd(func() int { return 1000 }))
	cmd.SetArgs([]string{"--config", missing})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Errorf("err = %v, want an unprivileged run to reach the config", err)
	}
}
