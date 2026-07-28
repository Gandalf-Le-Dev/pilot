package transport

import (
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "''"},
		{"simple", "simple"},
		{"/opt/pilot/services/api", "/opt/pilot/services/api"},
		{"0042-9f3ac1b", "0042-9f3ac1b"},
		{"with space", "'with space'"},
		{"semi;colon", "'semi;colon'"},
		{"$(whoami)", "'$(whoami)'"},
		{"back`tick`", "'back`tick`'"},
		{"it's", `'it'\''s'`},
		{"a&&b", "'a&&b'"},
		{"*.caddy", "'*.caddy'"},
	}
	for _, tc := range tests {
		if got := Quote(tc.in); got != tc.want {
			t.Errorf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A service name or path is data; it must never become shell syntax.
func TestQuoteNeutralisesInjection(t *testing.T) {
	cmd := "rm -rf " + Quote("/opt/pilot/services/x; rm -rf /")
	if strings.Contains(cmd, "; rm -rf /'") == false && strings.Contains(cmd, "; rm") {
		// The dangerous substring must be inside quotes, not bare.
		if !strings.HasPrefix(cmd, "rm -rf '") {
			t.Errorf("injection not neutralised: %s", cmd)
		}
	}
	if !strings.HasSuffix(cmd, "'") {
		t.Errorf("expected a quoted argument, got: %s", cmd)
	}
}
func TestJoinAndScript(t *testing.T) {
	if got := Join("docker", "compose", "-p", "my app", "up", "-d"); got != "docker compose -p 'my app' up -d" {
		t.Errorf("Join = %q", got)
	}
	got := Script("echo one\necho two\n")
	if !strings.HasPrefix(got, "set -eu\n") {
		t.Errorf("scripts must abort on first failure, got:\n%s", got)
	}
	// pipefail is bash-only; dash is /bin/sh on Debian and rejects it outright,
	// taking the whole script down. It has to be conditional.
	if strings.Contains(got, "set -euo pipefail") {
		t.Errorf("the bash-only form would fail on dash:\n%s", got)
	}
	if !strings.Contains(got, "(set -o pipefail) 2>/dev/null") {
		t.Errorf("pipefail should still be attempted where supported:\n%s", got)
	}
	if !strings.HasSuffix(got, "echo two\n") {
		t.Errorf("script body mangled:\n%s", got)
	}
}
