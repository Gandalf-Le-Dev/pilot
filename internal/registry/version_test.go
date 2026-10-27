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
		{"v18.13.3", "18.24.2", true}, // the prefix is spelling, not identity
		{"1.2.3", "v1.3.0", true},

		{"16-alpine", "17", false},            // different variant
		{"16", "17-alpine", false},            // different variant
		{"v16-alpine", "17", false},           // different variant across spellings
		{"v18.13.3", "18.24.2-alpine", false}, // different variant across spellings
		{"3.0.4", "3.0", false},               // a track, not a release
		{"3.0.4", "3", false},                 // a track, not a release
		{"v3.0.4", "3.0", false},              // a track, not a release
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

// atuin published `v18.13.3` and then switched to bare `18.14.0` onwards, so
// requiring a matching prefix reported it as current for eleven releases.
func TestLatestAcrossPrefixSpellings(t *testing.T) {
	tests := []struct {
		name    string
		current string
		tags    []string
		want    string
		step    Step
	}{
		{
			name:    "atuin dropped the v",
			current: "v18.13.3",
			tags:    []string{"v18.13.3", "18.14.0", "18.24.2", "18.24.2-1", "18.24.0-beta.1", "18.24", "latest"},
			want:    "18.24.2",
			step:    StepMinor,
		},
		{
			name:    "project added a v",
			current: "1.2.3",
			tags:    []string{"1.2.3", "v1.3.0"},
			want:    "v1.3.0",
			step:    StepMinor,
		},
		{
			name:    "newest under both spellings keeps the bare spelling in use",
			current: "1.2.3",
			tags:    []string{"v2.0.0", "2.0.0", "v1.2.3"},
			want:    "2.0.0",
			step:    StepMajor,
		},
		{
			name:    "newest under both spellings keeps the v spelling in use",
			current: "v1.2.3",
			tags:    []string{"v1.2.4", "1.2.4", "1.2.3"},
			want:    "v1.2.4",
			step:    StepPatch,
		},
		{
			name:    "variant suffix still separates series",
			current: "v1.2.3-alpine",
			tags:    []string{"1.9.0", "v1.9.0", "1.2.4-alpine", "v1.2.3-alpine"},
			want:    "1.2.4-alpine",
			step:    StepPatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			current, _ := ParseVersion(tc.current)
			got, found := Latest(current, tc.tags)
			if !found || got.Raw != tc.want {
				t.Fatalf("Latest = %q (found %v), want %q", got.Raw, found, tc.want)
			}
			if step := current.StepTo(got); step != tc.step {
				t.Errorf("StepTo = %q, want %q", step, tc.step)
			}
		})
	}
}

// The same release under the other spelling is not an update; reporting it
// would ask somebody to redeploy the image they already run.
func TestLatestIgnoresSameReleaseUnderOtherSpelling(t *testing.T) {
	for _, tc := range []struct {
		current string
		tags    []string
	}{
		{"v1.2.3", []string{"v1.2.3", "1.2.3", "1.2.2"}},
		{"1.2.3", []string{"1.2.3", "v1.2.3", "v1.2.2"}},
		{"1.2.3", []string{"v1.2.3"}},
	} {
		current, _ := ParseVersion(tc.current)
		if got, found := Latest(current, tc.tags); found {
			t.Errorf("Latest(%s, %v) = %q, want no update", tc.current, tc.tags, got.Raw)
		}
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
		{"v18.13.3", "18.24.2", StepMinor},
		{"1.2.3", "v2.0.0", StepMajor},
		{"v1.2.3", "1.2.3", StepNone},
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
