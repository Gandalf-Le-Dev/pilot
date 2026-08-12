package registry

import "testing"

// Tags taken from what ghcr and Docker Hub actually return for the images this
// fleet runs. Most of a registry's tags are not versions, and treating one of
// them as a version means telling somebody to deploy a branch build.
func TestParseVersionRejectsNonVersions(t *testing.T) {
	for _, tag := range []string{
		"latest", "beta", "alpine", "alpine3.21",
		"feature-remote-ocr-workflow", "fix-13627", "chore-pnpm-11",
		"perm-filter-unify-filter-backends", "ngx-1.7.0",
		"", "sha-abc123",
	} {
		if v, ok := ParseVersion(tag); ok {
			t.Errorf("ParseVersion(%q) accepted it as %+v", tag, v)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		tag    string
		prefix string
		parts  []int
		suffix string
		pre    string
	}{
		{"3.0.4", "", []int{3, 0, 4}, "", ""},
		{"v18.12.0", "v", []int{18, 12, 0}, "", ""},
		{"17", "", []int{17}, "", ""},
		{"7.2-alpine", "", []int{7, 2}, "-alpine", ""},
		{"16-alpine", "", []int{16}, "-alpine", ""},
		{"9.6.4-alpine", "", []int{9, 6, 4}, "-alpine", ""},
		{"2.18.0-rc1", "", []int{2, 18, 0}, "", "rc1"},
	}
	for _, tc := range tests {
		t.Run(tc.tag, func(t *testing.T) {
			v, ok := ParseVersion(tc.tag)
			if !ok {
				t.Fatalf("ParseVersion(%q) rejected a version", tc.tag)
			}
			if v.Prefix != tc.prefix || v.Suffix != tc.suffix || v.Pre != tc.pre {
				t.Errorf("got prefix=%q suffix=%q pre=%q, want %q/%q/%q",
					v.Prefix, v.Suffix, v.Pre, tc.prefix, tc.suffix, tc.pre)
			}
			if len(v.Parts) != len(tc.parts) {
				t.Fatalf("parts = %v, want %v", v.Parts, tc.parts)
			}
			for i := range v.Parts {
				if v.Parts[i] != tc.parts[i] {
					t.Errorf("parts = %v, want %v", v.Parts, tc.parts)
				}
			}
		})
	}
}

// A variant suffix is part of the image's identity. Offering `17` as an update
// to `16-alpine` would swap the base distribution under a running service.
func TestComparableRespectsVariantAndShape(t *testing.T) {
	v := func(s string) Version {
		p, ok := ParseVersion(s)
		if !ok {
			t.Fatalf("could not parse %q", s)
		}
		return p
	}

	tests := []struct {
		a, b string
		want bool
	}{
		{"3.0.4", "3.0.5", true},
		{"3.0.4", "3.1.2", true},
		{"16-alpine", "17-alpine", true},
		{"v18.12.0", "v18.12.1", true},

		{"16-alpine", "17", false},     // different variant
		{"16", "17-alpine", false},     // different variant
		{"v18.12.0", "18.12.1", false}, // different prefix convention
		{"3.0.4", "3.0", false},        // a track, not a release
		{"3.0.4", "3", false},          // a track, not a release
	}
	for _, tc := range tests {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			if got := v(tc.a).Comparable(v(tc.b)); got != tc.want {
				t.Errorf("Comparable = %v, want %v", got, tc.want)
			}
		})
	}
}

// Docker Hub returns tags in lexical order — ['10','11',...,'18','8','9'] — so
// picking "the last one" would report postgres 9 as the newest release.
func TestLatestIgnoresRegistryOrdering(t *testing.T) {
	current, _ := ParseVersion("14")
	tags := []string{"10", "11", "12", "13", "14", "15", "16", "17", "18", "8", "9", "latest", "alpine"}

	got, found := Latest(current, tags)
	if !found || got.Raw != "18" {
		t.Errorf("Latest = %q (found %v), want 18", got.Raw, found)
	}
}

// Somebody on a stable release should not be moved onto a release candidate.
func TestLatestExcludesPreReleases(t *testing.T) {
	current, _ := ParseVersion("2.17.5")
	tags := []string{"2.17.5", "2.18.0-rc1", "2.18.0-rc2"}

	if got, found := Latest(current, tags); found {
		t.Errorf("Latest = %q, want no update from a stable release", got.Raw)
	}

	// But someone already on an rc wants to hear about the next one — and once
	// the real release lands it should win over any remaining candidate, since
	// a pre-release precedes the release it leads to.
	onRC, _ := ParseVersion("2.18.0-rc1")
	if got, found := Latest(onRC, tags); !found || got.Raw != "2.18.0-rc2" {
		t.Errorf("Latest from rc1 = %q (found %v), want rc2", got.Raw, found)
	}

	released := append(tags, "2.18.0")
	if got, found := Latest(onRC, released); !found || got.Raw != "2.18.0" {
		t.Errorf("Latest from rc1 = %q (found %v), want the 2.18.0 release to beat rc2", got.Raw, found)
	}
}

func TestLatestFindsNothingWhenCurrent(t *testing.T) {
	current, _ := ParseVersion("0.95.0")
	if got, found := Latest(current, []string{"0.94.0", "0.95.0", "latest"}); found {
		t.Errorf("Latest = %q, want no update", got.Raw)
	}
}

func TestStepTo(t *testing.T) {
	tests := []struct {
		from, to string
		want     Step
	}{
		{"3.0.4", "3.0.5", StepPatch},
		{"3.0.4", "3.1.2", StepMinor},
		{"3.0.4", "4.0.0", StepMajor},
		{"3.0.4", "3.0.4", StepNone},
		{"v18.12.0", "v18.12.1", StepPatch},
		// Two components leave no room for a patch: the second place is the
		// smallest thing that can move, so it is a minor.
		{"7.2", "7.3", StepMinor},
		{"16", "17", StepMajor},
	}
	for _, tc := range tests {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			from, _ := ParseVersion(tc.from)
			to, _ := ParseVersion(tc.to)
			if got := from.StepTo(to); got != tc.want {
				t.Errorf("StepTo = %q, want %q", got, tc.want)
			}
		})
	}
}

// A tag naming a series rather than a release is chosen precisely so it moves.
// Reporting "postgres 18 is available" daily for a major migration nobody will
// do on a Tuesday is how a checker becomes something you stop reading.
func TestIsTrack(t *testing.T) {
	for tag, want := range map[string]bool{
		"17":         true,
		"8":          true,
		"7.2-alpine": true,
		"16-alpine":  true,
		"3.0.4":      false,
		"v18.12.0":   false,
		"0.95.0":     false,
	} {
		v, ok := ParseVersion(tag)
		if !ok {
			t.Fatalf("could not parse %q", tag)
		}
		if got := v.IsTrack(); got != want {
			t.Errorf("%q IsTrack = %v, want %v", tag, got, want)
		}
	}
}
