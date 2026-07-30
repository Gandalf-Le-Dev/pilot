package redact

import (
	"strings"
	"testing"
)

// TestScanFindsTheRealLeak is the case this package was written for: a wakapi
// access log carrying the user's API key in the request URI.
func TestScanFindsTheRealLeak(t *testing.T) {
	line := `wakapi | INFO [request] status="201" method="POST" ` +
		`uri="/api/users/current/heartbeats.bulk?api_key=26dba175-be35-4919-886a-4ab0509c1a07" ` +
		`duration="15.6ms" user="Gandalf"`

	got := Scan(line, nil)
	if len(got) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(got), got)
	}
	if got[0].Kind != KindLabelled || got[0].Label != "api_key" {
		t.Errorf("got %+v, want a labelled api_key match", got[0])
	}
}

// TestMatchesNeverCarryTheSecret is the property that matters most. A report
// that quoted the value would copy the credential into whatever reads the
// report — a terminal, a JSON document, a CI log.
func TestMatchesNeverCarryTheSecret(t *testing.T) {
	secret := "26dba175-be35-4919-886a-4ab0509c1a07"
	text := "GET /x?api_key=" + secret + " and password: sup3rs3cret!"

	for _, m := range Scan(text, map[string]string{"DB_PASSWORD": "sup3rs3cret!"}) {
		if strings.Contains(m.Label, secret) || strings.Contains(m.Label, "sup3rs3cret") {
			t.Errorf("match leaks the value it is reporting: %+v", m)
		}
	}
}

// TestKnownValuesReportedByName covers secrets Pilot supplied itself, where no
// guessing is involved — the value is matched exactly and named by its variable.
func TestKnownValuesReportedByName(t *testing.T) {
	known := map[string]string{
		"DOCMOST_DB_PASSWORD": "hunter2hunter2",
		"SHORT":               "abc",          // below minKnownLength
		"UNRESOLVED":          "${cmd:thing}", // a reference, not a value
	}
	text := "connecting to postgres with hunter2hunter2 ok; abc; ${cmd:thing}"

	got := Scan(text, known)
	if len(got) != 1 {
		t.Fatalf("got %d matches, want only the resolved one: %+v", len(got), got)
	}
	if got[0].Kind != KindKnown || got[0].Label != "DOCMOST_DB_PASSWORD" {
		t.Errorf("got %+v, want DOCMOST_DB_PASSWORD", got[0])
	}
}

// TestPilotsOwnIdentifiersAreNotSecrets is the reason entropy scanning was
// rejected. Every one of these is high-entropy and completely ordinary; a
// threshold-based detector would report Pilot's own output as a credential leak
// and get switched off within a day.
func TestPilotsOwnIdentifiersAreNotSecrets(t *testing.T) {
	ordinary := []string{
		"activated release 0042-9f3ac1b",
		"commit 4cdc33da573bf1e2a9c8d4e5f6a7b8c9d0e1f2a3",
		"container 3f8a9c2b1d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a",
		"request id 550e8400-e29b-41d4-a716-446655440000",
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"GET /api/heartbeats?start=2026-07-30&end=2026-07-31 200",
		"listening on /run/pilot.sock (protocol 3, build 0.3.0)",
	}
	for _, line := range ordinary {
		if got := Scan(line, nil); len(got) != 0 {
			t.Errorf("false positive on %q: %+v", line, got)
		}
	}
}

// TestPlaceholdersAreIgnored — a check that reports already-redacted output
// teaches people to ignore it.
func TestPlaceholdersAreIgnored(t *testing.T) {
	for _, line := range []string{
		`api_key=<redacted>`,
		`password: ****`,
		`token="null"`,
		`secret=REDACTED`,
		`client_secret: changeme`,
	} {
		if got := Scan(line, nil); len(got) != 0 {
			t.Errorf("reported a placeholder in %q: %+v", line, got)
		}
	}
}

func TestScanRecognisesUnambiguousFormats(t *testing.T) {
	// The fixtures are assembled rather than written out, and that is not
	// squeamishness: the first version of this test used the vendors' own
	// documentation examples and GitHub's push protection rejected the commit.
	// A file full of realistic credentials trips every other scanner in the
	// toolchain, which is a fair complaint about a file, so these are obviously
	// synthetic while still matching the patterns under test.
	fake := strings.Repeat("A", 24)
	cases := map[string]string{
		"JWT":               "auth eyJ" + fake + "." + "eyJzdWIiOiIx" + "." + "c2lnbmF0dXJl",
		"AWS access key ID": "using AKIA" + strings.Repeat("Q", 16) + " for s3",
		"PEM private key":   "-----BEGIN RSA PRIVATE KEY-----",
		"GitHub token":      "cloning with ghp_" + fake,
		"Slack token":       "posting with xoxb-" + fake,
		"Stripe key":        "charge failed sk_" + "test_" + fake,
	}
	for want, line := range cases {
		got := Scan(line, nil)
		if len(got) != 1 || got[0].Label != want || got[0].Kind != KindFormat {
			t.Errorf("Scan(%q) = %+v, want one %s format match", line, got, want)
		}
	}
}

func TestScanCountsOccurrences(t *testing.T) {
	text := "a?api_key=aaaaaaaa\nb?api_key=bbbbbbbb\nc?api_key=cccccccc"
	got := Scan(text, nil)
	if len(got) != 1 || got[0].Count != 3 {
		t.Fatalf("got %+v, want a single match with count 3", got)
	}
}

// TestScanIsStable — an unstable report reads as a change when nothing changed.
func TestScanIsStable(t *testing.T) {
	text := "api_key=aaaaaaaa token=bbbbbbbb password=cccccccc AKIA" + strings.Repeat("Q", 16)
	first := Scan(text, nil)
	for range 20 {
		got := Scan(text, nil)
		if len(got) != len(first) {
			t.Fatalf("unstable length: %d then %d", len(first), len(got))
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("unstable order at %d: %+v vs %+v", i, first[i], got[i])
			}
		}
	}
}

func TestSummariseNamesLabelsOnly(t *testing.T) {
	awsKey := "AKIA" + strings.Repeat("Q", 16)
	s := Summarise(Scan("api_key=aaaaaaaa "+awsKey, nil))
	for _, want := range []string{"api_key", "AWS access key ID"} {
		if !strings.Contains(s, want) {
			t.Errorf("Summarise() = %q, missing %q", s, want)
		}
	}
	if strings.Contains(s, awsKey) {
		t.Errorf("Summarise() leaked a value: %q", s)
	}
}

// TestIsSecretNameRejectsLocations is a regression test for a false positive
// found on the first real run: every literal environment value was searched for
// in the service's own logs, so `BASE_URL` was reported as a leaked credential
// the moment the service logged its own base URL.
func TestIsSecretNameRejectsLocations(t *testing.T) {
	secrets := []string{
		"DB_PASSWORD", "APP_SECRET", "WAKAPI_PASSWORD_SALT", "GITHUB_TOKEN",
		"api_key", "OVH_APPLICATION_SECRET", "AWS_ACCESS_KEY", "AUTH_TOKEN",
	}
	for _, name := range secrets {
		if !IsSecretName(name) {
			t.Errorf("IsSecretName(%q) = false, want true", name)
		}
	}

	// Locations and plain configuration. A location in a log line is ordinary.
	notSecrets := []string{
		"BASE_URL", "APP_URL", "PORT", "TZ", "LOG_LEVEL", "POSTGRES_DB",
		"SECRET_FILE", "PRIVATE_KEY_PATH", "TOKEN_URI", "SERVICE_NAME", "TENANT_ID",
	}
	for _, name := range notSecrets {
		if IsSecretName(name) {
			t.Errorf("IsSecretName(%q) = true, want false", name)
		}
	}
}
