package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
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

func discardLog(*slog.Logger) {}

func TestServerRefusesRootBeforeReadingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	cmd := quiet(newServerCmd(func() int { return 0 }, discardLog))
	cmd.SetArgs([]string{"--config", missing})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "refusing to run as root") {
		t.Fatalf("err = %v, want the root refusal before the missing file is noticed", err)
	}

	cmd = quiet(newServerCmd(func() int { return 1000 }, discardLog))
	cmd.SetArgs([]string{"--config", missing})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Errorf("err = %v, want an unprivileged run to reach the config", err)
	}
}

// The daemons' fatal error is the line an operator most needs from
// `journalctl -p err`, so it goes through their log at error level, and main
// must not print it a second time. ctl's stderr goes back to the CLI, so it
// keeps the plain form and never installs the daemon log.
func TestDaemonsLogTheirFatalErrorOnce(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent")

	tests := map[string]struct {
		args      []string
		installed bool
		logged    string
		printed   string
	}{
		"serve":  {[]string{"serve", "--root", notDir}, true, "pilotd: level=ERROR msg=exiting err=", ""},
		"server": {[]string{"server", "--config", missing + ".yaml"}, true, "pilotd server: level=ERROR msg=exiting err=", ""},
		"ctl":    {[]string{"ctl", "info", "--socket", missing + ".sock"}, false, "", "pilotd: "},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var installed *slog.Logger
			var stderr bytes.Buffer
			root := newRoot(func() int { return 1000 }, func(l *slog.Logger) { installed = l })
			root.SetOut(io.Discard)
			root.SetErr(&stderr)
			root.SetArgs(tt.args)

			err := root.Execute()
			if err == nil {
				t.Fatal("want an error")
			}
			if got := installed != nil; got != tt.installed {
				t.Errorf("installed a default log = %v, want %v", got, tt.installed)
			}
			report(&stderr, err)

			out := stderr.String()
			if n := strings.Count(out, "\n"); n != 1 {
				t.Errorf("wrote %d lines, want the error once:\n%s", n, out)
			}
			if tt.logged != "" && !strings.HasPrefix(out, tt.logged) {
				t.Errorf("got %q, want it logged as %q...", out, tt.logged)
			}
			if tt.printed != "" && (!strings.HasPrefix(out, tt.printed) || strings.Contains(out, "level=")) {
				t.Errorf("got %q, want the plain %q form", out, tt.printed)
			}
		})
	}
}

func TestReportPrintsUnloggedErrors(t *testing.T) {
	var buf bytes.Buffer
	report(&buf, errors.New("boom"))
	if got, want := buf.String(), "pilotd: boom\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
