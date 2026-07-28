package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveEnv(t *testing.T) {
	t.Setenv("PILOT_TEST_SALT", "s3cret")

	tests := []struct{ in, want string }{
		{"plain value", "plain value"},
		{"${env:PILOT_TEST_SALT}", "s3cret"},
		{"prefix-${env:PILOT_TEST_SALT}-suffix", "prefix-s3cret-suffix"},
		{"${env:PILOT_TEST_SALT}${env:PILOT_TEST_SALT}", "s3crets3cret"},
		{"", ""},
	}
	for _, tc := range tests {
		got, err := Resolve(tc.in)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveReportsMissingEnv(t *testing.T) {
	_, err := Resolve("${env:PILOT_DEFINITELY_NOT_SET}")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "not set in this shell") {
		t.Errorf("error should say where to look: %v", err)
	}
}

// An empty value is almost never intended and would ship as an empty password.
func TestResolveRejectsEmptyEnv(t *testing.T) {
	t.Setenv("PILOT_TEST_EMPTY", "")
	if _, err := Resolve("${env:PILOT_TEST_EMPTY}"); err == nil {
		t.Error("an empty variable should be an error, not an empty secret")
	}
}

// The whole point: an unresolvable reference must never pass through as a
// literal. That would deploy cleanly and fail somewhere far from the cause.
func TestUnimplementedSchemesFailLoudly(t *testing.T) {
	for _, ref := range []string{
		"${sops:secrets/prod.yaml#api.database_url}",
		"${op:vault/item/field}",
	} {
		got, err := Resolve(ref)
		if err == nil {
			t.Fatalf("Resolve(%q) should have failed, got %q", ref, got)
		}
		if strings.Contains(got, "${") {
			t.Errorf("a literal reference leaked into the result: %q", got)
		}
		if !strings.Contains(err.Error(), "${env:") {
			t.Errorf("error should suggest the workaround: %v", err)
		}
	}
}

func TestUnknownSchemeListsTheKnownOnes(t *testing.T) {
	_, err := Resolve("${nonsense:x}")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "env") {
		t.Errorf("error should list what is available: %v", err)
	}
}

// Being told about one missing variable, fixing it, and being told about the
// next is a poor way to spend a deploy.
func TestResolveMapReportsEveryProblemAtOnce(t *testing.T) {
	_, err := ResolveMap(map[string]string{
		"A": "${env:PILOT_MISSING_ONE}",
		"B": "${env:PILOT_MISSING_TWO}",
		"C": "fine",
	})
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "PILOT_MISSING_ONE") || !strings.Contains(msg, "PILOT_MISSING_TWO") {
		t.Errorf("both problems should be reported together:\n%s", msg)
	}
	if !strings.Contains(msg, "2 values") {
		t.Errorf("should count them: %s", msg)
	}
}

func TestResolveMapPassesThroughPlainValues(t *testing.T) {
	t.Setenv("PILOT_TEST_SALT", "s3cret")

	got, err := ResolveMap(map[string]string{
		"LOG_LEVEL": "info",
		"SALT":      "${env:PILOT_TEST_SALT}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["LOG_LEVEL"] != "info" || got["SALT"] != "s3cret" {
		t.Errorf("got %v", got)
	}
}

func TestHasRef(t *testing.T) {
	if HasRef("plain") {
		t.Error("plain value should not look like a reference")
	}
	if !HasRef("${env:X}") {
		t.Error("reference not detected")
	}
}

func TestResolveCommand(t *testing.T) {
	got, err := Resolve("${cmd:printf s3cret}")
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret" {
		t.Errorf("got %q", got)
	}
}

// `security find-generic-password -w` adds a newline. A stray newline inside a
// password salt is a miserable thing to debug.
func TestCommandOutputIsTrimmed(t *testing.T) {
	got, err := Resolve("${cmd:printf 's3cret\\n'}")
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret" {
		t.Errorf("got %q — trailing newline not stripped", got)
	}
}

func TestCommandFailureIsReported(t *testing.T) {
	_, err := Resolve("${cmd:exit 1}")
	if err == nil {
		t.Fatal("want an error")
	}
	if _, err := Resolve("${cmd:printf ''}"); err == nil {
		t.Error("a command producing nothing should be an error, not an empty secret")
	}
}

func TestResolveFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "salt")
	if err := os.WriteFile(p, []byte("from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve("${file:" + p + "}")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-a-file" {
		t.Errorf("got %q", got)
	}

	if _, err := Resolve("${file:" + dir + "/nope}"); err == nil {
		t.Error("a missing file should be an error")
	}
}
