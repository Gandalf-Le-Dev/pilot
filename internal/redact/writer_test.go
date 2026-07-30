package redact

import (
	"strings"
	"testing"
)

// TestTextRemovesTheRealLeakButKeepsTheLine is the case this exists for.
//
// The URI, the status, the method and the user all survive — the line is still
// worth reading — and only the key is gone.
func TestTextRemovesTheRealLeakButKeepsTheLine(t *testing.T) {
	secret := "26dba175-be35-4919-886a-4ab0509c1a07"
	line := `INFO [request] status="201" method="POST" ` +
		`uri="/api/users/current/heartbeats.bulk?api_key=` + secret + `" user="Gandalf"`

	got := Text(line, nil)

	if strings.Contains(got, secret) {
		t.Fatalf("the credential survived redaction:\n%s", got)
	}
	if !strings.Contains(got, "api_key="+Placeholder) {
		t.Errorf("the label should survive so the line stays debuggable:\n%s", got)
	}
	for _, keep := range []string{`status="201"`, `method="POST"`, "heartbeats.bulk", `user="Gandalf"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction removed %q, which is not a secret:\n%s", keep, got)
		}
	}
}

func TestTextRemovesKnownValuesByExactMatch(t *testing.T) {
	known := map[string]string{"DB_PASSWORD": "hunter2hunter2"}
	got := Text("postgres: authenticated with hunter2hunter2", known)

	if strings.Contains(got, "hunter2hunter2") {
		t.Fatalf("known value survived: %s", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Errorf("expected a placeholder: %s", got)
	}
}

func TestTextLeavesOrdinaryOutputAlone(t *testing.T) {
	// The same lines the scanner must not flag. Redaction that mangles a release
	// ID or a commit SHA destroys the reason to read logs at all.
	for _, line := range []string{
		"activated release 0042-9f3ac1b",
		"commit 4cdc33da573bf1e2a9c8d4e5f6a7b8c9d0e1f2a3",
		"GET /api/heartbeats?start=2026-07-30&end=2026-07-31 200",
		"listening on /run/pilot.sock (protocol 3, build 0.3.0)",
	} {
		if got := Text(line, nil); got != line {
			t.Errorf("Text(%q) = %q, want it unchanged", line, got)
		}
	}
}

// TestTextIsIdempotent — logs get re-read, and a second pass must not turn
// `<redacted>` into something else or start nesting placeholders.
func TestTextIsIdempotent(t *testing.T) {
	line := `api_key=aaaaaaaa password: bbbbbbbb token="cccccccc"`
	once := Text(line, nil)
	twice := Text(once, nil)
	if once != twice {
		t.Errorf("not idempotent:\n first: %s\nsecond: %s", once, twice)
	}
}

// TestWriterRedactsAcrossWriteBoundaries is why the Writer buffers by line.
//
// Log output arrives in whatever sizes the transport chooses, so a credential
// can straddle two Write calls. Redacting each chunk as it came would let the
// halves through untouched.
func TestWriterRedactsAcrossWriteBoundaries(t *testing.T) {
	secret := "26dba175-be35-4919-886a-4ab0509c1a07"
	var out strings.Builder
	w := NewWriter(&out, nil)

	// Split mid-secret, exactly where a naive implementation fails.
	if _, err := w.Write([]byte("GET /x?api_key=" + secret[:10])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(secret[10:] + " 200\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if strings.Contains(got, secret) || strings.Contains(got, secret[10:]) {
		t.Fatalf("a split credential survived: %q", got)
	}
	if !strings.Contains(got, "200") {
		t.Errorf("the rest of the line was lost: %q", got)
	}
}

// TestWriterFlushesLineWithoutNewline — the last line of a crashed service has
// no trailing newline, and it is the one worth reading.
func TestWriterFlushesLineWithoutNewline(t *testing.T) {
	var out strings.Builder
	w := NewWriter(&out, nil)

	if _, err := w.Write([]byte("panic: something broke")); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("emitted an incomplete line early: %q", out.String())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "panic: something broke" {
		t.Errorf("Close() produced %q", got)
	}
}

func TestWriterPreservesLineStructure(t *testing.T) {
	var out strings.Builder
	w := NewWriter(&out, nil)
	in := "one\ntwo\nthree\n"
	if _, err := w.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

// TestWriterDoesNotBufferForever guards the memory cap: a service emitting no
// newlines must not be able to grow the buffer without limit.
func TestWriterDoesNotBufferForever(t *testing.T) {
	var out strings.Builder
	w := NewWriter(&out, nil)

	chunk := strings.Repeat("x", 64<<10)
	for range 32 { // 2 MiB, no newline anywhere
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if out.Len() == 0 {
		t.Error("nothing was flushed; the buffer grew past the cap unchecked")
	}
	if len(w.buf) > maxLine {
		t.Errorf("buffer is %d bytes, above the %d cap", len(w.buf), maxLine)
	}
}
