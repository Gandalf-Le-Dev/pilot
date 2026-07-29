// Package selfupdate replaces the running pilot binary with a newer release.
//
// It deliberately refuses to do that when a package manager owns the binary.
// Overwriting a Homebrew-managed file leaves brew's bookkeeping describing
// something that is no longer there, and the next `brew upgrade` fights the
// change — so the honest move is to name the command that actually works.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/install"
)

const (
	Owner = "Gandalf-Le-Dev"
	Repo  = "pilot"

	timeout = 3 * time.Minute
)

// APIBase and DownloadBase are overridable for tests.
var (
	APIBase      = "https://api.github.com"
	DownloadBase = fmt.Sprintf("https://github.com/%s/%s/releases/download", Owner, Repo)
)

// Method is how the running binary was installed.
type Method int

const (
	MethodUnknown Method = iota
	MethodHomebrew
	MethodManual
)

// Detect reports how the binary at path was installed.
//
// Symlinks are resolved first: Homebrew links `pilot` into its bin directory,
// so the interesting path is the target, not the link.
func Detect(path string) Method {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	switch {
	case strings.Contains(real, "/Caskroom/"), strings.Contains(real, "/Cellar/"):
		return MethodHomebrew
	case real != "":
		return MethodManual
	}
	return MethodUnknown
}

// Release is the subset of a GitHub release Pilot cares about.
type Release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

// Version is the tag without its leading v.
func (r Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// Latest fetches the most recent release.
func Latest(ctx context.Context) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", APIBase, Owner, Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checking for releases: %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("the latest release has no tag")
	}
	return &rel, nil
}

// Newer reports whether b is a later version than a.
//
// A deliberately small comparison: these are tags this project cut, so
// major.minor.patch with an optional pre-release suffix covers every case, and
// a dependency for it would not.
func Newer(a, b string) bool { return compare(b, a) > 0 }

func compare(x, y string) int {
	xs, xpre := splitPre(strings.TrimPrefix(x, "v"))
	ys, ypre := splitPre(strings.TrimPrefix(y, "v"))

	for i := range 3 {
		if d := at(xs, i) - at(ys, i); d != 0 {
			if d > 0 {
				return 1
			}
			return -1
		}
	}
	// Equal cores: a pre-release sorts before the plain version.
	switch {
	case xpre == "" && ypre != "":
		return 1
	case xpre != "" && ypre == "":
		return -1
	case xpre > ypre:
		return 1
	case xpre < ypre:
		return -1
	}
	return 0
}

func splitPre(v string) ([]string, string) {
	core, pre, _ := strings.Cut(v, "-")
	return strings.Split(core, "."), pre
}

func at(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.Atoi(parts[i])
	return n
}

// AssetName is the archive published for this platform.
func AssetName(version string) string {
	return fmt.Sprintf("pilot_%s_%s_%s.tar.gz", strings.TrimPrefix(version, "v"), runtime.GOOS, runtime.GOARCH)
}

// Apply downloads a release and replaces the binary at exePath.
//
// The new binary is checksum-verified and then *executed* before it replaces
// anything: a file whose digest matches can still be the wrong architecture or
// a truncated extract, and discovering that after the swap would leave no
// working pilot to fix it with.
func Apply(ctx context.Context, rel *Release, exePath string, log func(string, ...any)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	asset := AssetName(rel.Version())
	base := fmt.Sprintf("%s/%s", DownloadBase, rel.TagName)

	log("  verifying %s", asset)
	sums, err := fetch(ctx, base+"/"+install.ChecksumFile)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", install.ChecksumFile, err)
	}
	want, ok := install.ChecksumFor(string(sums), asset)
	if !ok {
		return fmt.Errorf("%s does not list %s — this platform may not be published",
			install.ChecksumFile, asset)
	}

	log("  downloading %s", rel.TagName)
	body, err := fetch(ctx, base+"/"+asset)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", asset, err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s\n  expected %s\n  got      %s\n"+
			"refusing to install a binary that does not match its release", asset, want, got)
	}

	bin, err := extract(body, "pilot")
	if err != nil {
		return err
	}

	// Stage beside the target so the final move is a rename on one filesystem,
	// which is atomic — a partially written binary is never observable.
	dir := filepath.Dir(exePath)
	tmp := filepath.Join(dir, ".pilot-upgrade-tmp")
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("cannot write to %s: %w\ntry: sudo pilot upgrade", dir, err)
		}
		return err
	}
	defer os.Remove(tmp)

	if err := verifyRuns(ctx, tmp, rel.Version()); err != nil {
		return err
	}

	log("  replacing %s", exePath)
	if err := os.Rename(tmp, exePath); err != nil {
		return fmt.Errorf("replacing %s: %w", exePath, err)
	}
	return nil
}

// verifyRuns executes the downloaded binary before trusting it.
func verifyRuns(ctx context.Context, path, wantVersion string) error {
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), wantVersion) {
		return fmt.Errorf("the downloaded binary reports %q, expected version %s",
			strings.TrimSpace(string(out)), wantVersion)
	}
	return nil
}

// extract pulls one file out of a .tar.gz.
func extract(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, fmt.Errorf("unreadable archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) == name && hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 256<<20))
		}
	}
	return nil, fmt.Errorf("%s not found in the archive", name)
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

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}
