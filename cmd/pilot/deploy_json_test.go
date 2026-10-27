package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSSH puts an `ssh` on PATH that runs each remote command on this machine,
// so a command can be driven end to end against a host that is not there.
//
// Agent calls answer "not found", as on a host with no agent. Running them
// locally would reach a real agent on a machine that is itself a Pilot host.
func fakeSSH(t *testing.T, host string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"for last; do :; done\n" +
		"case \"$last\" in\n" +
		"  *" + host + ") exit 0 ;;\n" +
		"  *\" ctl \"*|*pilotd*) echo 'pilotd: not found' >&2; exit 127 ;;\n" +
		"esac\n" +
		"exec /bin/sh -c \"$last\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// capture runs fn with stdout and stderr redirected, and returns both.
func capture(t *testing.T, fn func() error) (stdout, stderr string, err error) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	collect := func(f **os.File) func() string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		*f = w
		var buf bytes.Buffer
		done := make(chan struct{})
		go func() { _, _ = io.Copy(&buf, r); close(done) }()
		return func() string { w.Close(); <-done; return buf.String() }
	}
	stopOut := collect(&os.Stdout)
	stopErr := collect(&os.Stderr)

	err = fn()
	return stopOut(), stopErr(), err
}

// Found by running a plan against a real fleet: the build's progress line
// went to stdout ahead of the document, so `--plan --json` did not parse for
// any service with a build step.
func TestDeployPlanJSONIsTheOnlyThingOnStdout(t *testing.T) {
	// The fake ssh runs commands here, and on a Pilot host they would read
	// its real releases.
	if _, err := os.Stat("/opt/pilot"); err == nil {
		t.Skip("this machine is a Pilot host")
	}
	const host = "pilot-json-test.invalid"
	fakeSSH(t, host)

	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("fleet.yaml", "version: 1\nhosts:\n  web-1:\n    address: "+host+"\n    user: deploy\n")
	write("services/site/service.yaml", `runtime: static
hosts: [web-1]
build:
  command: mkdir -p dist && echo hello > dist/index.html
  output: [dist/]
expose:
  domains: [site.example.com]
  static: {index: index.html}
`)

	stdout, stderr, err := capture(t, func() error {
		root := buildRoot(&globals{})
		root.SetArgs([]string{"deploy", "site", "--plan", "--json", "--no-agent-upgrade", "-C", dir})
		return root.ExecuteContext(t.Context())
	})
	if err != nil {
		t.Fatalf("deploy --plan --json: %v\nstderr:\n%s", err, stderr)
	}

	var plan struct {
		Release string            `json:"release"`
		Hosts   []json.RawMessage `json:"hosts"`
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if plan.Release == "" || len(plan.Hosts) != 1 {
		t.Errorf("plan = %+v", plan)
	}
	if !strings.Contains(stderr, "building site") {
		t.Errorf("build progress should still reach stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "no usable agent on web-1") {
		t.Errorf("the agent call should have been refused, as on a host with none:\n%s", stderr)
	}
}
