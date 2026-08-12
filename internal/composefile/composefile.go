// Package composefile reads the parts of a compose file Pilot reasons about.
//
// It exists so there is one image-reference parser rather than one per caller.
// `doctor` needs the images to flag moving tags; `updates` needs them to ask a
// registry what is newer. Two regexes would drift, and the first sign would be
// one command seeing an image the other did not.
//
// This is deliberately not a compose parser. Pilot never interprets a compose
// file — Docker does — and a partial YAML model that looked authoritative would
// be worse than a regex that is obviously a regex.
package composefile

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

// DefaultFile is the compose file assumed when a service names none.
const DefaultFile = "compose.yaml"

// imageLine matches an `image:` entry.
var imageLine = regexp.MustCompile(`(?m)^\s*image:\s*["']?([^"'\s#]+)`)

// Images returns every image reference declared, sorted and deduplicated.
//
// Interpolated references are skipped: `${REGISTRY}/app:1.0` cannot be resolved
// without the environment the host will use, and guessing would produce
// confident nonsense.
func Images(body []byte) []string {
	seen := map[string]bool{}
	var out []string

	for _, m := range imageLine.FindAllStringSubmatch(string(body), -1) {
		ref := m[1]
		if strings.Contains(ref, "${") || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// Tag returns the tag part of an image reference, defaulting to `latest`.
//
// The colon in `registry.example.com:5000/app` is a port, not a tag separator,
// so only a colon in the final path segment counts.
func Tag(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref[i+1:]
	}
	return "latest"
}

// Read loads a service's compose file from the fleet root.
func Read(root string, s *config.Service) ([]byte, error) {
	name := DefaultFile
	if s.Compose != nil && s.Compose.File != "" {
		name = s.Compose.File
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
