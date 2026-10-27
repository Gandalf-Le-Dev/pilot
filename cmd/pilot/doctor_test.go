package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/doctor"
)

// `doctor --fix --json` prints the report on stdout, so the repair lines must
// go to the writer it is handed, never to stdout directly.
func TestApplyFixesWritesOnlyToItsWriter(t *testing.T) {
	report := &doctor.Report{Findings: []doctor.Finding{
		{Title: "stale fleet config", Host: "web-1", FixDesc: "push it",
			Fix: func(context.Context) error { return nil }},
		{Title: "no import line", FixDesc: "add it",
			Fix: func(context.Context) error { return errors.New("read-only Caddyfile") }},
		{Title: "nothing to repair"},
	}}

	var buf bytes.Buffer
	stdout, _, err := capture(t, func() error { return applyFixes(t.Context(), &buf, report, false) })
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	for _, want := range []string{"fixing web-1: stale fleet config — push it", "done", "fixing no import line — add it"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("progress lacks %q:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "nothing to repair") {
		t.Errorf("a finding with no fix is not progress:\n%s", buf.String())
	}
}
