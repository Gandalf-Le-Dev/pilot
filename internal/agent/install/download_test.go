package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssetNameMatchesGoreleaser(t *testing.T) {
	// This name is a contract with .goreleaser.yaml's name_template. If they
	// drift, bootstrap constructs a URL that 404s on every release.
	if got := AssetName(ArchAMD64); got != "pilotd-linux-amd64" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName(ArchARM64); got != "pilotd-linux-arm64" {
		t.Errorf("AssetName = %q", got)
	}
}

func TestReleaseTag(t *testing.T) {
	for in, want := range map[string]string{"0.1.0": "v0.1.0", "v0.1.0": "v0.1.0"} {
		if got := ReleaseTag(in); got != want {
			t.Errorf("ReleaseTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// A build with no matching release must fall back to compiling, not construct
// a URL that cannot exist.
func TestIsReleaseVersion(t *testing.T) {
	tests := map[string]bool{
		"0.1.0":       true,
		"v1.2.3":      true,
		"dev":         false,
		"":            false,
		"0.0.1-next":  false,
		"0.1.0-dirty": false,
	}
	for in, want := range tests {
		if got := IsReleaseVersion(in); got != want {
			t.Errorf("IsReleaseVersion(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	sums := `abc123  pilot_0.1.0_darwin_arm64.tar.gz
DEF456  pilotd-linux-amd64
789aaa  dist/pilotd-linux-arm64
`
	for asset, want := range map[string]string{
		"pilotd-linux-amd64": "def456", // normalised to lower case
		"pilotd-linux-arm64": "789aaa", // path prefix tolerated
	} {
		got, ok := ChecksumFor(sums, asset)
		if !ok || got != want {
			t.Errorf("ChecksumFor(%q) = %q, %v; want %q", asset, got, ok, want)
		}
	}
	if _, ok := ChecksumFor(sums, "nope"); ok {
		t.Error("an absent asset should not resolve")
	}
}

// fakeRelease serves a release, optionally corrupting it.
func fakeRelease(t *testing.T, body []byte, corruptSum bool, omitSum bool) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if corruptSum {
		digest = strings.Repeat("0", 64)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v0.1.0/"+ChecksumFile, func(w http.ResponseWriter, r *http.Request) {
		if omitSum {
			fmt.Fprintf(w, "%s  something-else\n", digest)
			return
		}
		fmt.Fprintf(w, "%s  %s\n", digest, AssetName(ArchAMD64))
	})
	mux.HandleFunc("/v0.1.0/"+AssetName(ArchAMD64), func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	return httptest.NewServer(mux)
}

func withCacheDir(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestDownloadVerifiesAndCaches(t *testing.T) {
	withCacheDir(t)
	payload := []byte("ELF-ish agent bytes")
	srv := fakeRelease(t, payload, false, false)
	defer srv.Close()

	src := Source{Version: "0.1.0", BaseURL: srv.URL}
	path, origin, err := src.download(context.Background(), ArchAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(origin, "checksum verified") {
		t.Errorf("origin should say it was verified, got %q", origin)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("downloaded content wrong: %q, %v", got, err)
	}
	if fi, _ := os.Stat(path); fi != nil && fi.Mode().Perm()&0o111 == 0 {
		t.Error("downloaded agent should be executable")
	}

	// Second call is served from cache, without touching the server.
	srv.Close()
	path2, origin2, err := src.download(context.Background(), ArchAMD64)
	if err != nil {
		t.Fatalf("cached fetch should not need the network: %v", err)
	}
	if path2 != path || !strings.Contains(origin2, "cached") {
		t.Errorf("expected a cache hit, got %q / %q", path2, origin2)
	}
}

// This binary is about to run as root on a server. A mismatch is refused, and
// nothing is left behind for a later run to pick up.
func TestDownloadRefusesChecksumMismatch(t *testing.T) {
	withCacheDir(t)
	srv := fakeRelease(t, []byte("agent"), true, false)
	defer srv.Close()

	src := Source{Version: "0.1.0", BaseURL: srv.URL}
	_, _, err := src.download(context.Background(), ArchAMD64)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error should name the problem: %v", err)
	}

	entries, _ := os.ReadDir(CacheDir())
	for _, e := range entries {
		t.Errorf("a rejected download was left in the cache: %s", e.Name())
	}
}

func TestDownloadRefusesUnlistedAsset(t *testing.T) {
	withCacheDir(t)
	srv := fakeRelease(t, []byte("agent"), false, true)
	defer srv.Close()

	src := Source{Version: "0.1.0", BaseURL: srv.URL}
	if _, _, err := src.download(context.Background(), ArchAMD64); err == nil {
		t.Error("an asset absent from checksums.txt must not be installed unverified")
	}
}

func TestDownloadRefusesDevBuild(t *testing.T) {
	withCacheDir(t)
	src := Source{Version: "dev"}
	_, _, err := src.download(context.Background(), ArchAMD64)
	if err == nil {
		t.Fatal("a dev build has no release to fetch from")
	}
	if !strings.Contains(err.Error(), "no matching release") {
		t.Errorf("error should explain: %v", err)
	}
}

// A dev build inside a checkout should compile rather than reach for a URL
// that cannot exist.
func TestResolveFallsBackToBuildingForDevBuilds(t *testing.T) {
	withCacheDir(t)
	_, _, cleanup, err := Source{Version: "dev"}.Resolve(context.Background(), ArchAMD64)
	defer cleanup()
	if err == nil {
		t.Fatal("want an error with no module and no release")
	}
	if !strings.Contains(err.Error(), "no matching release") {
		t.Errorf("error should say why a download was not attempted: %v", err)
	}
	if !strings.Contains(err.Error(), "GOARCH=amd64") {
		t.Errorf("error should give the build command: %v", err)
	}
}

func TestCachePathIsVersioned(t *testing.T) {
	withCacheDir(t)
	a := cachePath("0.1.0", ArchAMD64)
	b := cachePath("0.2.0", ArchAMD64)
	if a == b {
		t.Error("upgrading Pilot must fetch a fresh agent, not reuse the previous one")
	}
	if filepath.Base(a) != "pilotd-v0.1.0-linux-amd64" {
		t.Errorf("cache name = %q", filepath.Base(a))
	}
}
