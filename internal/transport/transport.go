// Package transport defines how Pilot executes work on a host, independent of
// whether it gets there over SSH or is already running on the box.
//
// The Executor interface is what lets phase 2 exist at all: the agent reuses
// every runtime adapter unchanged, because a runtime never knew whether its
// commands were travelling over a network in the first place.
package transport

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Result is the outcome of one command.
//
// A non-zero ExitCode is data, not an error: `test -f x` returning 1 is a
// perfectly good answer. Only failures to execute at all come back as errors.
type Result struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

// OK reports whether the command succeeded.
func (r Result) OK() bool { return r.ExitCode == 0 }

// Out returns trimmed standard output.
func (r Result) Out() string { return strings.TrimSpace(r.Stdout) }

// Err converts a non-zero exit into an error, for call sites that require
// success, using stderr as the message when there is one.
func (r Result) Err() error {
	if r.OK() {
		return nil
	}
	msg := strings.TrimSpace(r.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(r.Stdout)
	}
	if msg == "" {
		return fmt.Errorf("command exited %d", r.ExitCode)
	}
	return fmt.Errorf("command exited %d: %s", r.ExitCode, msg)
}

// Executor runs commands and moves files on one host.
//
// Both the SSH client and the agent's in-process executor implement it, so a
// runtime adapter works identically whether it is driven from a laptop or from
// the daemon on the machine itself.
type Executor interface {
	Run(ctx context.Context, cmd string) (Result, error)
	RunScript(ctx context.Context, body string) (Result, error)
	Stream(ctx context.Context, cmd string, stdout, stderr io.Writer) (int, error)

	// RunInput runs a command with stdin supplied. This is how a request
	// reaches `pilotd ctl` — on stdin rather than in the argument vector, so
	// there is no length limit and nothing sensitive lands in the host's
	// process list.
	RunInput(ctx context.Context, cmd string, stdin []byte) (Result, error)

	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, mode string) error
	Exists(ctx context.Context, path string) (bool, error)
	HasCommand(ctx context.Context, name string) (bool, error)

	UploadDir(ctx context.Context, localDir, remoteDir string) error
	MkdirAll(ctx context.Context, dir string) error
	RemoveAll(ctx context.Context, path string) error

	// Label identifies the host in messages.
	Label() string
}

// Swapper is implemented by executors that can perform the atomic release
// swap in-process rather than by shelling out.
//
// The agent satisfies it. That matters for more than tidiness: the shell form
// depends on `mv -T`, which is GNU-only, whereas rename(2) through the standard
// library is both portable and unambiguously atomic — and forking a shell to
// move a symlink on the machine you are already running on is silly.
type Swapper interface {
	Swap(serviceDir, releaseID string) error
}

// Quote renders s as a single shell word.
//
// Everything Pilot sends is wrapped this way rather than interpolated, so a
// service name or path containing a space, a quote, or a semicolon stays data
// and never becomes syntax.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isShellSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == '/', c == ':', c == '=', c == '@', c == ',', c == '+':
		default:
			return false
		}
	}
	return true
}

// Join renders a command and its arguments as one quoted shell line.
func Join(argv ...string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = Quote(a)
	}
	return strings.Join(out, " ")
}

// Script wraps a multi-line script so it aborts on the first failure and on an
// unset variable, and on a pipeline failure where the shell supports it.
//
// `set -eu` is POSIX and works everywhere. `pipefail` is not: Debian and Ubuntu
// use dash as /bin/sh, which rejects `set -euo pipefail` outright and takes the
// whole script down with it. So it is enabled conditionally — bash and zsh get
// it, dash carries on without.
func Script(body string) string {
	const preamble = "set -eu\n" +
		"(set -o pipefail) 2>/dev/null && set -o pipefail || true\n"
	return preamble + strings.TrimRight(body, "\n") + "\n"
}

// FirstLine returns the first non-empty line of s, for one-line error summaries.
func FirstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(head)
}
