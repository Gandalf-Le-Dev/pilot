// Package secrets expands ${scheme:argument} references in configuration
// values.
//
// Pilot deliberately implements references rather than a secret store: the
// value lives wherever you already keep it, and the fleet config records only
// how to find it.
//
// A reference Pilot cannot resolve is an error, never a passthrough. Shipping
// the literal string "${sops:secrets.yaml#db.password}" as a database password
// would appear to work — the deploy succeeds, the container starts — and fail
// somewhere far away from the cause.
package secrets

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// refPattern matches ${scheme:argument}.
var refPattern = regexp.MustCompile(`\$\{([a-zA-Z][a-zA-Z0-9_]*):([^}]*)\}`)

// Scheme is a way of locating a value.
type Scheme string

const (
	// SchemeEnv reads from the operator's environment at deploy time, which
	// includes anything loaded from the fleet's .env file.
	SchemeEnv Scheme = "env"

	// SchemeCommand runs a command and takes its output. This is the escape
	// hatch that makes every other secret store reachable — a keychain, pass,
	// Vault, 1Password — without Pilot integrating with any of them.
	SchemeCommand Scheme = "cmd"

	// SchemeFile reads a value from a file, for secrets already sitting on
	// disk with restrictive permissions.
	SchemeFile Scheme = "file"

	// Not yet implemented. Named so the error can say what was meant rather
	// than "unknown scheme".
	SchemeSops    Scheme = "sops"
	SchemeOnePass Scheme = "op"
)

var pending = map[Scheme]string{
	SchemeSops:    "SOPS/age decryption",
	SchemeOnePass: "the 1Password CLI",
}

// commandTimeout bounds a ${cmd:...} lookup. A keychain prompt that never gets
// answered should fail the deploy rather than hang it forever.
const commandTimeout = 30 * time.Second

// Resolve expands every reference in a value.
//
// A value with no references is returned unchanged, so this is safe to run over
// plain configuration.
func Resolve(value string) (string, error) {
	var firstErr error

	out := refPattern.ReplaceAllStringFunc(value, func(match string) string {
		m := refPattern.FindStringSubmatch(match)
		scheme, arg := Scheme(m[1]), m[2]

		resolved, err := lookup(scheme, arg)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return resolved
	})

	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

func lookup(scheme Scheme, arg string) (string, error) {
	switch scheme {
	case SchemeEnv:
		v, ok := os.LookupEnv(arg)
		if !ok {
			return "", fmt.Errorf("${env:%s} is not set in this shell or in the fleet's .env", arg)
		}
		if v == "" {
			return "", fmt.Errorf("${env:%s} is set but empty", arg)
		}
		return v, nil

	case SchemeCommand:
		return runCommand(arg)

	case SchemeFile:
		b, err := os.ReadFile(expandHome(arg))
		if err != nil {
			return "", fmt.Errorf("${file:%s}: %w", arg, err)
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return "", fmt.Errorf("${file:%s} is empty", arg)
		}
		return v, nil
	}

	if what, known := pending[scheme]; known {
		return "", fmt.Errorf("${%s:%s} needs %s, which this build does not implement yet\n"+
			"for now, export it and use ${env:...} instead", scheme, arg, what)
	}
	return "", fmt.Errorf("unknown reference scheme %q in ${%s:%s} (known: %s)",
		scheme, scheme, arg, strings.Join(knownSchemes(), ", "))
}

// runCommand executes a lookup command and returns its output.
//
// It runs through a shell so pipes and quoting work the way they do when you
// test the command by hand — which matters, because these are commands you
// arrive at by trying them in a terminal first.
//
// Note that this makes the fleet config executable: anything in a ${cmd:...}
// runs on the machine performing the deploy. That is the intended trade for
// reaching any secret store without Pilot integrating with it, and it is safe
// on the assumption that you wrote your own fleet config.
func runCommand(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			return "", fmt.Errorf("${cmd:%s} timed out after %s", command, commandTimeout)
		}
		if detail != "" {
			return "", fmt.Errorf("${cmd:%s} failed: %s", command, firstLine(detail))
		}
		return "", fmt.Errorf("${cmd:%s} failed: %w", command, err)
	}

	// Trailing newlines are an artefact of the command, not part of the
	// secret. `security find-generic-password -w` adds one, and a stray
	// newline in a password salt is a miserable thing to debug.
	v := strings.TrimRight(stdout.String(), "\r\n")
	if v == "" {
		return "", fmt.Errorf("${cmd:%s} produced no output", command)
	}
	return v, nil
}

func expandHome(p string) string {
	if after, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, after)
		}
	}
	return p
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return head
}

func knownSchemes() []string {
	out := []string{string(SchemeEnv)}
	for s := range pending {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

// ResolveMap expands every value in a map.
//
// Errors are collected and reported together: being told about one missing
// variable, fixing it, and immediately being told about the next is a poor way
// to spend a deploy.
func ResolveMap(in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))

	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var problems []string
	for _, k := range keys {
		v, err := Resolve(in[k])
		if err != nil {
			problems = append(problems, fmt.Sprintf("  %s: %v", k, err))
			continue
		}
		out[k] = v
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("could not resolve %s:\n%s",
			plural(len(problems), "value"), strings.Join(problems, "\n"))
	}
	return out, nil
}

// HasRef reports whether a value contains any reference, so callers can tell
// "resolved to nothing" from "was never a reference".
func HasRef(value string) bool { return refPattern.MatchString(value) }

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
