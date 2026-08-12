package registry

import (
	"regexp"
	"strconv"
	"strings"
)

// Version is a container tag parsed into something comparable.
//
// Registries hand back everything: release versions, branch names, `latest`,
// `beta`, and one-off build tags. paperless-ngx alone publishes 208 tags of
// which most are things like `feature-remote-ocr-workflow`. So parsing has to
// reject far more than it accepts, and the rejects must not silently become
// version zero — a tag that failed to parse is not an old release.
type Version struct {
	// Prefix is "v" or empty. `v18.12.0` and `18.12.0` are both common and a
	// project uses one or the other, never both for the same release.
	Prefix string

	// Parts are the numeric components, most significant first.
	Parts []int

	// Suffix is a variant marker like "-alpine" or "-slim". It is part of the
	// tag's identity, not decoration: `16-alpine` and `16` are different
	// images, and offering one as an update to the other would swap the base
	// distribution underneath a running service.
	Suffix string

	// Pre is a pre-release marker such as "rc1" or "beta.2".
	Pre string

	// Raw is the tag exactly as the registry reported it.
	Raw string
}

// tagPattern splits a tag into prefix, digits, pre-release and suffix.
//
// Deliberately strict. Anything that is not recognisably a version is dropped
// rather than guessed at, because the cost of a wrong guess here is telling
// somebody to upgrade to a branch build.
var tagPattern = regexp.MustCompile(
	`^(v?)(\d+(?:\.\d+)*)` + // prefix and dotted numbers
		`(?:-(alpha|beta|rc|pre)[.\-]?(\d*))?` + // optional pre-release
		`(-[a-z][a-z0-9.]*)?$`) // optional variant suffix, e.g. -alpine

// ParseVersion reads a tag, reporting whether it is a version at all.
func ParseVersion(tag string) (Version, bool) {
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}

	var parts []int
	for _, p := range strings.Split(m[2], ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, false
		}
		parts = append(parts, n)
	}

	v := Version{Prefix: m[1], Parts: parts, Suffix: m[5], Raw: tag}
	if m[3] != "" {
		v.Pre = m[3] + m[4]
	}
	return v, true
}

// IsTrack reports whether the tag names a moving series rather than one release.
//
// `postgres:17` and `redis:8` are tracks: they are meant to move under you, and
// that is why they were chosen. Treating them as pins would mean reporting
// "postgres 18 is available" every single day for a major upgrade nobody is
// going to do on a Tuesday — and a checker that cries about work you will not
// do is one you stop reading, which then hides the update that mattered.
func (v Version) IsTrack() bool { return len(v.Parts) < 3 }

// Comparable reports whether other names a release in the same series as v.
//
// Same prefix, same variant suffix, and the same number of components. That
// last one is what keeps `3.0` and `3` out of the comparison for a service
// pinned to `3.0.4`: they are tracks pointing at a release, not releases.
func (v Version) Comparable(other Version) bool {
	return v.Prefix == other.Prefix &&
		v.Suffix == other.Suffix &&
		len(v.Parts) == len(other.Parts)
}

// Newer reports whether other is a later release than v.
func (v Version) Newer(other Version) bool { return compare(v, other) < 0 }

// compare orders two versions, -1 if a < b.
func compare(a, b Version) int {
	for i := range a.Parts {
		if i >= len(b.Parts) {
			return 1
		}
		switch {
		case a.Parts[i] < b.Parts[i]:
			return -1
		case a.Parts[i] > b.Parts[i]:
			return 1
		}
	}
	if len(b.Parts) > len(a.Parts) {
		return -1
	}

	// A pre-release precedes the release it leads to: 2.18.0-rc1 < 2.18.0.
	switch {
	case a.Pre != "" && b.Pre == "":
		return -1
	case a.Pre == "" && b.Pre != "":
		return 1
	case a.Pre < b.Pre:
		return -1
	case a.Pre > b.Pre:
		return 1
	}
	return 0
}

// Step describes how far apart two releases are.
type Step string

const (
	StepNone  Step = "current"
	StepPatch Step = "patch"
	StepMinor Step = "minor"
	StepMajor Step = "major"
)

// StepTo classifies the distance from v to other.
//
// Read from the left: the first component that moved decides. For a two-part
// version, a change in the second component is a minor rather than a patch —
// there is no third place for a patch to live.
func (v Version) StepTo(other Version) Step {
	for i := range v.Parts {
		if i >= len(other.Parts) || v.Parts[i] == other.Parts[i] {
			continue
		}
		switch i {
		case 0:
			return StepMajor
		case 1:
			return StepMinor
		default:
			return StepPatch
		}
	}
	if v.Pre != other.Pre {
		return StepPatch
	}
	return StepNone
}

// Latest returns the newest tag in the same series as current.
//
// Pre-releases are excluded unless the deployed tag is itself a pre-release —
// somebody running 2.18.0-rc1 wants to know about rc2, and somebody running
// 2.17.5 does not want to be moved onto a release candidate.
func Latest(current Version, tags []string) (Version, bool) {
	best, found := current, false

	for _, tag := range tags {
		v, ok := ParseVersion(tag)
		if !ok || !current.Comparable(v) {
			continue
		}
		if v.Pre != "" && current.Pre == "" {
			continue
		}
		if best.Newer(v) {
			best, found = v, true
		}
	}
	return best, found
}
