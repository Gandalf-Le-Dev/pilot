package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/composefile"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

// imageLine matches an `image:` entry in a compose file.
//
// Deliberately a regex rather than a compose parser. Pilot does not otherwise
// interpret compose files — it hands them to Docker — and adopting a schema for
// one warning would mean tracking Compose's own evolution forever. A line this
// check cannot read is a line it stays quiet about.
var imageLine = regexp.MustCompile(`(?m)^\s*image:\s*["']?([^"'\s#]+)`)

// checkImageTags reports images pinned to a tag that moves.
//
// Two things break when a compose file says `:latest`, and both are silent:
//
//   - A deploy becomes a no-op. The release hash covers the compose file's
//     *content*, so publishing a new image under the same tag changes nothing
//     Pilot can see, and `pilot deploy` correctly reports that there is nothing
//     to do — while the new version never ships.
//   - A rollback stops rolling back. Images are pulled at stage time and not at
//     activation, so re-activating an earlier release runs whatever `latest`
//     resolves to *now*. The release directory goes back; the software does not.
//
// The second is the dangerous one. It reports success and changes nothing, which
// is the worst way for a recovery mechanism to fail — and it fails at the moment
// it is needed.
func checkImageTags(ctx context.Context, env *Env) []Finding {
	var out []Finding

	for _, name := range env.Fleet.ServiceNames() {
		s := env.Fleet.Services[name]
		if s.Compose == nil || !s.Deployable() {
			continue
		}

		body, err := readComposeFile(env.Fleet.Root, s)
		if err != nil {
			continue // unreadable is not this check's problem to report
		}

		unpinned := unpinnedImages(body)
		if len(unpinned) == 0 {
			continue
		}

		out = append(out, Finding{
			Status: StatusWarn, Scope: ScopeConfig,
			Title: fmt.Sprintf("%s deploys an unpinned image (%s)", name, strings.Join(unpinned, ", ")),
			Detail: "A moving tag makes a new version invisible to Pilot: the release hash covers " +
				"the compose file, so publishing under the same tag changes nothing and `pilot deploy` " +
				"reports there is nothing to do. Rollback is worse — images are pulled when a release " +
				"is staged, not when it is activated, so returning to an earlier release runs whatever " +
				"the tag points at now. It succeeds and changes nothing.",
			Hint: "pin the tag, e.g. `image: " + firstImageName(unpinned[0]) + ":1.4.2`, or use a digest",
		})
	}
	return out
}

// readComposeFile loads a service's compose file from the fleet checkout.
//
// Local rather than from the host, so this works under `--offline` and reports
// the problem before a deploy rather than after one.
func readComposeFile(root string, s *config.Service) ([]byte, error) {
	name := s.Compose.File
	if name == "" {
		name = "compose.yaml"
	}

	var dir string
	if s.Source != nil {
		dir = s.Source.Path
	}
	if dir == "" {
		// A service with no local source has nothing to read here.
		return nil, os.ErrNotExist
	}
	return os.ReadFile(filepath.Join(root, dir, name))
}

// unpinnedImages returns the images in a compose file whose tag moves.
//
// Only `latest`, explicit or implied. A tag like `1.25` is mutable too, but it
// changes when somebody decides it should, and flagging it would bury the two
// cases that are provably broken under noise nobody reads.
func unpinnedImages(body []byte) []string {
	seen := map[string]bool{}
	var out []string

	for _, ref := range composefile.Images(body) {
		if strings.Contains(ref, "@sha256:") {
			continue // a digest is as pinned as it gets
		}
		if composefile.Tag(ref) != "latest" {
			continue
		}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

// firstImageName strips any tag, for use in the suggested fix.
func firstImageName(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref[:i]
	}
	return ref
}
