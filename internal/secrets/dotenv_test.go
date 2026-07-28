package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnv(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DotenvFile), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadDotenv(t *testing.T) {
	dir := writeEnv(t, `# a comment

WAKAPI_PASSWORD_SALT=s3cret
export EXPORTED=yes
QUOTED="has spaces"
SINGLE='literal $notexpanded'
INLINE=value # trailing comment
EMPTY=
`, 0o600)

	for _, k := range []string{"WAKAPI_PASSWORD_SALT", "EXPORTED", "QUOTED", "SINGLE", "INLINE", "EMPTY"} {
		t.Setenv(k, "") // registers cleanup
		os.Unsetenv(k)
	}

	loaded, err := LoadDotenv(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 6 {
		t.Errorf("loaded %d keys: %v", len(loaded), loaded)
	}

	tests := map[string]string{
		"WAKAPI_PASSWORD_SALT": "s3cret",
		"EXPORTED":             "yes",
		"QUOTED":               "has spaces",
		"SINGLE":               "literal $notexpanded",
		"INLINE":               "value",
		"EMPTY":                "",
	}
	for k, want := range tests {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// An existing variable must win, so a one-off override on the command line
// works without editing the file — and so CI needs no .env at all.
func TestExistingEnvironmentWins(t *testing.T) {
	dir := writeEnv(t, "OVERRIDE_ME=from-file\n", 0o600)
	t.Setenv("OVERRIDE_ME", "from-shell")

	if _, err := LoadDotenv(dir); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("OVERRIDE_ME"); got != "from-shell" {
		t.Errorf("OVERRIDE_ME = %q, want the shell value to win", got)
	}
}

// The whole point of the file is that it holds secrets, and 0644 is what most
// editors will give it.
func TestWorldReadableDotenvIsRefused(t *testing.T) {
	dir := writeEnv(t, "SECRET=x\n", 0o644)

	_, err := LoadDotenv(dir)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error should give the fix: %v", err)
	}
}

func TestMissingDotenvIsFine(t *testing.T) {
	loaded, err := LoadDotenv(t.TempDir())
	if err != nil {
		t.Fatalf("most fleets have no .env: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("loaded %v", loaded)
	}
}

func TestParseDotenvLine(t *testing.T) {
	tests := []struct {
		in       string
		key, val string
		ok       bool
	}{
		{"", "", "", false},
		{"# comment", "", "", false},
		{"  # indented comment", "", "", false},
		{"KEY=value", "KEY", "value", true},
		{"export KEY=value", "KEY", "value", true},
		{"KEY = spaced", "KEY", "spaced", true},
		{`KEY="quoted value"`, "KEY", "quoted value", true},
		{`KEY="with \"escape\""`, "KEY", `with "escape"`, true},
		{"KEY='$literal'", "KEY", "$literal", true},
		{"KEY=value # comment", "KEY", "value", true},
		{"KEY=has#hash", "KEY", "has#hash", true},
	}
	for _, tc := range tests {
		k, v, ok := parseDotenvLine(tc.in)
		if ok != tc.ok || k != tc.key || v != tc.val {
			t.Errorf("parseDotenvLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, k, v, ok, tc.key, tc.val, tc.ok)
		}
	}
}
