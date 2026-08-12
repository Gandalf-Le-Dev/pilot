package registry

import "testing"

func TestParseRef(t *testing.T) {
	tests := []struct {
		image string
		reg   string
		repo  string
		tag   string
	}{
		// The forms this fleet actually uses.
		{"ghcr.io/paperless-ngx/paperless-ngx:3.0.4", "ghcr.io", "paperless-ngx/paperless-ngx", "3.0.4"},
		{"ghcr.io/muety/wakapi:2.17.5", "ghcr.io", "muety/wakapi", "2.17.5"},
		{"docmost/docmost:0.95.0", "docker.io", "docmost/docmost", "0.95.0"},
		{"docker.io/library/redis:8", "docker.io", "library/redis", "8"},

		// An official image carries an implicit library/ that the API needs
		// spelled out.
		{"postgres:14", "docker.io", "library/postgres", "14"},
		{"redis:7.2-alpine", "docker.io", "library/redis", "7.2-alpine"},

		// No tag means latest, the same default the daemon uses.
		{"postgres", "docker.io", "library/postgres", "latest"},

		// A registry port must not be mistaken for a tag.
		{"localhost:5000/team/app:1.2.3", "localhost:5000", "team/app", "1.2.3"},
		{"localhost:5000/team/app", "localhost:5000", "team/app", "latest"},
	}

	for _, tc := range tests {
		t.Run(tc.image, func(t *testing.T) {
			ref, err := ParseRef(tc.image)
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tc.image, err)
			}
			if ref.Registry != tc.reg || ref.Repo != tc.repo || ref.Tag != tc.tag {
				t.Errorf("got %s / %s / %s, want %s / %s / %s",
					ref.Registry, ref.Repo, ref.Tag, tc.reg, tc.repo, tc.tag)
			}
		})
	}
}

// A digest names exact bits and belongs to no version series — which is the
// entire reason somebody pins one. Reporting an "update" for it would be
// meaningless, so it is rejected rather than guessed at.
func TestParseRefRejectsDigests(t *testing.T) {
	_, err := ParseRef("ghcr.io/x/y@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err == nil {
		t.Error("a digest-pinned reference should be rejected")
	}
}

// A ghcr image and a Docker Hub image can share a repo path and be completely
// different images, so the host is only implicit for Docker Hub.
func TestRefName(t *testing.T) {
	for image, want := range map[string]string{
		"postgres:14":                   "postgres",
		"docmost/docmost:1.0":           "docmost/docmost",
		"ghcr.io/atuinsh/atuin:v18.1.0": "ghcr.io/atuinsh/atuin",
		"docker.io/library/redis:8":     "redis",
	} {
		ref, err := ParseRef(image)
		if err != nil {
			t.Fatal(err)
		}
		if got := ref.Name(); got != want {
			t.Errorf("Name() for %q = %q, want %q", image, got, want)
		}
	}
}

func TestRefString(t *testing.T) {
	for image, want := range map[string]string{
		"postgres:14":                   "postgres:14",
		"docmost/docmost:1.0":           "docmost/docmost:1.0",
		"ghcr.io/atuinsh/atuin:v18.1.0": "ghcr.io/atuinsh/atuin:v18.1.0",
	} {
		ref, err := ParseRef(image)
		if err != nil {
			t.Fatal(err)
		}
		if got := ref.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func TestNextLink(t *testing.T) {
	tests := map[string]string{
		`</v2/library/postgres/tags/list?n=1000&last=9.6>; rel="next"`: "/v2/library/postgres/tags/list?n=1000&last=9.6",
		`</v2/x/tags/list>; rel="prev"`:                                "",
		"":                                                             "",
		"garbage":                                                      "",
	}
	for header, want := range tests {
		if got := nextLink(header); got != want {
			t.Errorf("nextLink(%q) = %q, want %q", header, got, want)
		}
	}
}
