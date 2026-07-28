package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Where released agents come from.
const (
	ReleaseOwner = "Gandalf-Le-Dev"
	ReleaseRepo  = "pilot"

	// ChecksumFile is published alongside the binaries by GoReleaser.
	ChecksumFile = "checksums.txt"

	downloadTimeout = 2 * time.Minute
)

// DefaultBaseURL is the release download root. Overridable so the download
// path can be tested without reaching GitHub.
var DefaultBaseURL = fmt.Sprintf("https://github.com/%s/%s/releases/download", ReleaseOwner, ReleaseRepo)

// AssetName is the published name of an agent binary.
//
// It must agree with the `name_template` in .goreleaser.yaml — that agreement
// is the entire contract that lets a CLI find the agent built from its own
// commit.
func AssetName(arch Arch) string { return fmt.Sprintf("pilotd-linux-%s", arch) }

// ReleaseTag is the git tag for a version. GoReleaser strips the leading `v`
// when it injects the version, so it goes back on here.
func ReleaseTag(version string) string {
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

// IsReleaseVersion reports whether this build came from a tagged release.
//
// A `go build` leaves the placeholder, and there is no release to match it —
// such a build has to fall back to compiling the agent from source.
func IsReleaseVersion(v string) bool {
	return v != "" && v != "dev" && !strings.HasSuffix(v, "-next") && !strings.Contains(v, "dirty")
}

// CacheDir is where downloaded agents are kept between bootstraps.
func CacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pilot-agents")
	}
	return filepath.Join(home, ".pilot", "agents")
}

// cachePath is keyed by version and architecture, so upgrading Pilot fetches a
// fresh agent rather than silently reusing the previous one.
func cachePath(version string, arch Arch) string {
	return filepath.Join(CacheDir(), fmt.Sprintf("pilotd-%s-linux-%s", ReleaseTag(version), arch))
}

// download fetches the agent published with this CLI's own release.
//
// The binary is verified against the release's checksums.txt before it is
// cached or used. That check is not optional: this file is about to be
// installed as root on a server, so an unverifiable download is refused rather
// than trusted.
func (s Source) download(ctx context.Context, arch Arch) (path, origin string, err error) {
	if !IsReleaseVersion(s.Version) {
		return "", "", fmt.Errorf("this is a %q build, which has no matching release", orDefault(s.Version, "dev"))
	}

	cached := cachePath(s.Version, arch)
	if fi, err := os.Stat(cached); err == nil && fi.Size() > 0 {
		return cached, "cached from " + ReleaseTag(s.Version), nil
	}

	base := s.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	tag := ReleaseTag(s.Version)
	asset := AssetName(arch)

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	// Checksums first. Fetching the binary we then cannot verify would mean
	// deciding what to do with it, and the only safe answer is to discard it.
	sums, err := fetch(ctx, fmt.Sprintf("%s/%s/%s", base, tag, ChecksumFile))
	if err != nil {
		return "", "", fmt.Errorf("fetching %s for %s: %w", ChecksumFile, tag, err)
	}
	want, ok := ChecksumFor(string(sums), asset)
	if !ok {
		return "", "", fmt.Errorf("%s in release %s does not list %s", ChecksumFile, tag, asset)
	}

	body, err := fetch(ctx, fmt.Sprintf("%s/%s/%s", base, tag, asset))
	if err != nil {
		return "", "", fmt.Errorf("downloading %s from %s: %w", asset, tag, err)
	}

	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", "", fmt.Errorf("checksum mismatch for %s in %s\n  expected %s\n  got      %s\n"+
			"refusing to install a binary that does not match its release", asset, tag, want, got)
	}

	if err := os.MkdirAll(CacheDir(), 0o755); err != nil {
		return "", "", err
	}
	// Write then rename, so a cancelled download cannot leave a truncated
	// binary that a later run would happily install.
	tmp := cached + ".tmp"
	if err := os.WriteFile(tmp, body, 0o755); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, cached); err != nil {
		os.Remove(tmp)
		return "", "", err
	}

	return cached, "downloaded from " + tag + " (checksum verified)", nil
}

// ChecksumFor finds an asset's expected digest in a checksums.txt.
//
// The format is `<sha256>  <filename>`, one per line.
func ChecksumFor(checksums, asset string) (string, bool) {
	for line := range strings.SplitSeq(checksums, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		// The name may carry a path prefix depending on how it was generated.
		if filepath.Base(fields[1]) == asset {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found (%s)", url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
