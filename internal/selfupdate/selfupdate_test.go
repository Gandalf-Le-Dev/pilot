package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/install"
)

func TestNewer(t *testing.T) {
	tests := []struct {
		have, latest string
		want         bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.1.0", "0.1.1", true},
		{"0.1.0", "1.0.0", true},
		{"0.1.0", "0.1.0", false},
		{"0.2.0", "0.1.0", false},
		{"1.0.0", "0.9.9", false},
		// Tags may or may not carry the v.
		{"v0.1.0", "0.2.0", true},
		{"0.1.0", "v0.2.0", true},
		// Numeric, not lexical: 0.10.0 is after 0.9.0.
		{"0.9.0", "0.10.0", true},
		{"0.10.0", "0.9.0", false},
		// A pre-release precedes its final version.
		{"0.2.0-rc1", "0.2.0", true},
		{"0.2.0", "0.2.0-rc1", false},
		// Missing components read as zero.
		{"1.0", "1.0.1", true},
	}
	for _, tc := range tests {
		if got := Newer(tc.have, tc.latest); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.have, tc.latest, got, tc.want)
		}
	}
}

// Overwriting a Homebrew-managed binary leaves brew describing a file that is
// no longer what it thinks, and the next `brew upgrade` undoes the change.
func TestDetectHomebrew(t *testing.T) {
	tests := map[string]Method{
		"/opt/homebrew/Caskroom/pilot/0.1.0/pilot": MethodHomebrew,
		"/usr/local/Cellar/pilot/0.1.0/bin/pilot":  MethodHomebrew,
		"/usr/local/bin/pilot":                     MethodManual,
		"/home/me/.local/bin/pilot":                MethodManual,
	}
	for path, want := range tests {
		if got := Detect(path); got != want {
			t.Errorf("Detect(%q) = %v, want %v", path, got, want)
		}
	}
}

// Homebrew links its binary into bin/, so the link target is what matters.
func TestDetectFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	caskroom := filepath.Join(dir, "Caskroom", "pilot", "0.1.0")
	if err := os.MkdirAll(caskroom, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(caskroom, "pilot")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "pilot")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if got := Detect(link); got != MethodHomebrew {
		t.Errorf("Detect through a symlink = %v, want Homebrew", got)
	}
}

func TestAssetName(t *testing.T) {
	got := AssetName("0.1.0")
	want := fmt.Sprintf("pilot_0.1.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if got != want {
		t.Errorf("AssetName = %q, want %q", got, want)
	}
	if AssetName("v0.1.0") != want {
		t.Error("a leading v should not reach the asset name")
	}
}

// makeArchive builds a .tar.gz holding a runnable stub named `pilot`.
func makeArchive(t *testing.T, version string) []byte {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\necho \"pilot version %s\"\n", version)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{"README.md", "docs"},
		{"pilot", script},
	} {
		tw.WriteHeader(&tar.Header{
			Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg,
		})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func fakeRelease(t *testing.T, version string, corrupt bool) *httptest.Server {
	t.Helper()
	archive := makeArchive(t, version)
	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	if corrupt {
		digest = strings.Repeat("0", 64)
	}
	asset := AssetName(version)

	mux := http.NewServeMux()
	mux.HandleFunc("/v"+version+"/"+install.ChecksumFile, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", digest, asset)
	})
	mux.HandleFunc("/v"+version+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	return httptest.NewServer(mux)
}

func TestApplyReplacesTheBinary(t *testing.T) {
	srv := fakeRelease(t, "0.2.0", false)
	defer srv.Close()
	old := DownloadBase
	DownloadBase = srv.URL
	defer func() { DownloadBase = old }()

	exe := filepath.Join(t.TempDir(), "pilot")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	rel := &Release{TagName: "v0.2.0"}
	if err := Apply(context.Background(), rel, exe, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "0.2.0") {
		t.Errorf("binary not replaced: %q", got)
	}
	// No staging file left behind.
	entries, _ := os.ReadDir(filepath.Dir(exe))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pilot-upgrade") {
			t.Errorf("temporary file survived: %s", e.Name())
		}
	}
}

// A mismatch must leave the working binary in place — there would be no pilot
// left to fix it with otherwise.
func TestApplyRefusesChecksumMismatch(t *testing.T) {
	srv := fakeRelease(t, "0.2.0", true)
	defer srv.Close()
	old := DownloadBase
	DownloadBase = srv.URL
	defer func() { DownloadBase = old }()

	exe := filepath.Join(t.TempDir(), "pilot")
	os.WriteFile(exe, []byte("#!/bin/sh\necho old\n"), 0o755)

	err := Apply(context.Background(), &Release{TagName: "v0.2.0"}, exe, func(string, ...any) {})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error should name the problem: %v", err)
	}
	if got, _ := os.ReadFile(exe); !strings.Contains(string(got), "old") {
		t.Error("the working binary was replaced despite a bad checksum")
	}
}

// A digest can match and the binary still be unusable — wrong architecture, a
// truncated extract. Running it first catches that while there is still a
// working pilot to fall back on.
func TestApplyRefusesABinaryThatDoesNotRun(t *testing.T) {
	// The archive claims 0.9.9 but the release says 0.2.0.
	archive := makeArchive(t, "9.9.9")
	sum := sha256.Sum256(archive)
	asset := AssetName("0.2.0")

	mux := http.NewServeMux()
	mux.HandleFunc("/v0.2.0/"+install.ChecksumFile, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
	})
	mux.HandleFunc("/v0.2.0/"+asset, func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	old := DownloadBase
	DownloadBase = srv.URL
	defer func() { DownloadBase = old }()

	exe := filepath.Join(t.TempDir(), "pilot")
	os.WriteFile(exe, []byte("#!/bin/sh\necho old\n"), 0o755)

	err := Apply(context.Background(), &Release{TagName: "v0.2.0"}, exe, func(string, ...any) {})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "reports") {
		t.Errorf("error should report the mismatch: %v", err)
	}
	if got, _ := os.ReadFile(exe); !strings.Contains(string(got), "old") {
		t.Error("the working binary was replaced by one that reports the wrong version")
	}
}

func TestLatestReportsNoReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	if _, err := Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "no releases") {
		t.Errorf("got %v", err)
	}
}

func TestReleaseVersionStripsV(t *testing.T) {
	if got := (Release{TagName: "v1.2.3"}).Version(); got != "1.2.3" {
		t.Errorf("Version = %q", got)
	}
}
