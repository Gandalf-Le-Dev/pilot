package deploy

import (
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
)

// A plan is printed to a terminal and may end up in CI output, so it must name
// which variables changed without ever revealing a value.
func TestDescribeEnvChangeNeverLeaksValues(t *testing.T) {
	tests := []struct {
		name          string
		before, after []string
		want          string
	}{
		{"added", []string{"A"}, []string{"A", "FEATURE_QUEUE"}, "+FEATURE_QUEUE"},
		{"removed", []string{"A", "OLD"}, []string{"A"}, "-OLD"},
		{"both", []string{"A", "OLD"}, []string{"A", "NEW"}, "+NEW, -OLD"},
		{"value changed only", []string{"A"}, []string{"A"}, "values changed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := describeEnvChange(tc.before, tc.after)
			if got != tc.want {
				t.Errorf("describeEnvChange = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	prev := &release.Manifest{
		Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaaaaaaaaaabbbb"}},
		EnvKeys:   []string{"LOG_LEVEL"},
		EnvHash:   "env-1",
		RouteHash: "route-1",
	}

	t.Run("image change is reported by short digest", func(t *testing.T) {
		p := &Plan{
			Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:ccccccccccccdddd"}},
			EnvHash:   "env-1", RouteHash: "route-1",
		}
		got := diff(prev, p)
		if len(got) != 1 || got[0].Field != "image" {
			t.Fatalf("diff = %+v", got)
		}
		if strings.Contains(got[0].From, "sha256:") {
			t.Errorf("digests should be shortened for display, got %q", got[0].From)
		}
		if got[0].To != "cccccccccccc" {
			t.Errorf("To = %q", got[0].To)
		}
	})

	t.Run("identical release yields no changes", func(t *testing.T) {
		p := &Plan{
			Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaaaaaaaaaabbbb"}},
			EnvHash:   "env-1", RouteHash: "route-1",
		}
		if got := diff(prev, p); len(got) != 0 {
			t.Errorf("diff = %+v, want none", got)
		}
	})

	t.Run("route change is detected", func(t *testing.T) {
		p := &Plan{
			Artifacts: []release.Artifact{{Name: "image", Digest: "sha256:aaaaaaaaaaaabbbb"}},
			EnvHash:   "env-1", RouteHash: "route-2",
		}
		got := diff(prev, p)
		if len(got) != 1 || got[0].Field != "route" {
			t.Errorf("diff = %+v", got)
		}
	})
}

func TestPlanNoOp(t *testing.T) {
	tests := []struct {
		name  string
		hosts []HostPlan
		want  bool
	}{
		{"all unchanged", []HostPlan{{NoOp: true}, {NoOp: true}}, true},
		{"one changed", []HostPlan{{NoOp: true}, {NoOp: false}}, false},
		{"none", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plan{Hosts: tc.hosts}
			if got := p.NoOp(); got != tc.want {
				t.Errorf("NoOp = %v, want %v", got, tc.want)
			}
		})
	}
}

// The manifest travels to the host and is read back by `pilot releases`, so it
// records env names and a digest but never a value.
func TestPlanManifestOmitsSecretValues(t *testing.T) {
	p := &Plan{
		Service: &config.Service{
			Name: "api", Runtime: config.RuntimeCompose,
			Source: &config.Source{Repo: "git@github.com:me/api.git", Ref: "main"},
		},
		Release: "0042-9f3ac1b",
		Hash:    "9f3ac1bdeadbeef",
		Commit:  "abc123",
		EnvKeys: []string{"DATABASE_URL", "LOG_LEVEL"},
		EnvHash: release.HashMap(map[string]string{"DATABASE_URL": "postgres://u:secret@db/app"}),
	}

	m := p.Manifest("web-1")
	if err := m.Validate(); err != nil {
		t.Fatalf("generated manifest is invalid: %v", err)
	}
	if m.Sequence != 42 {
		t.Errorf("sequence = %d, want 42 from the release id", m.Sequence)
	}
	if m.Source == nil || m.Source.Commit != "abc123" {
		t.Errorf("source not recorded: %+v", m.Source)
	}

	body, err := release.MarshalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "secret") || strings.Contains(string(body), "postgres://") {
		t.Errorf("manifest leaked an env value:\n%s", body)
	}
	if !strings.Contains(string(body), "DATABASE_URL") {
		t.Error("manifest should record env names")
	}
}

func TestRenderRoute(t *testing.T) {
	layout := release.NewLayout("")

	t.Run("unexposed service has no route", func(t *testing.T) {
		got, err := renderRoute(&config.Service{Name: "worker"}, layout)
		if err != nil || got != "" {
			t.Errorf("got %q, %v", got, err)
		}
	})

	t.Run("static route is rooted at the current symlink", func(t *testing.T) {
		s := &config.Service{
			Name:    "blog",
			Runtime: config.RuntimeStatic,
			Expose: &config.Expose{
				Domains: []string{"blog.example.com"},
				Static:  &config.StaticExpose{Index: "index.html"},
			},
		}
		got, err := renderRoute(s, layout)
		if err != nil {
			t.Fatal(err)
		}
		// Rooting at `current` rather than a release path is what makes a
		// swap take effect without touching Caddy at all.
		if !strings.Contains(got, "root * /opt/pilot/services/blog/current") {
			t.Errorf("route should point at the symlink:\n%s", got)
		}
		if strings.Contains(got, "releases/") {
			t.Errorf("route must not pin a release directory:\n%s", got)
		}
	})
}

// Two deploys with identical inputs must produce the same hash, so that
// "nothing actually changed" is visible rather than silently re-shipped.
func TestReleaseHashIsStableForIdenticalInputs(t *testing.T) {
	hashFor := func(env map[string]string, digest string) string {
		h := release.NewHasher()
		h.AddString("runtime", "compose")
		h.AddString("env", release.HashMap(env))
		h.AddString("route", "route-1")
		h.AddString("artifact:image", digest)
		return h.Sum()
	}

	base := hashFor(map[string]string{"LOG_LEVEL": "info"}, "sha256:aaa")
	same := hashFor(map[string]string{"LOG_LEVEL": "info"}, "sha256:aaa")
	if base != same {
		t.Error("identical inputs produced different release hashes")
	}

	if base == hashFor(map[string]string{"LOG_LEVEL": "debug"}, "sha256:aaa") {
		t.Error("an env change must change the release hash")
	}
	if base == hashFor(map[string]string{"LOG_LEVEL": "info"}, "sha256:bbb") {
		t.Error("an image change must change the release hash")
	}
}

func TestShortDigest(t *testing.T) {
	tests := []struct{ in, want string }{
		{"sha256:aaaaaaaaaaaabbbbcccc", "aaaaaaaaaaaa"},
		{"short", "short"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := shortDigest(tc.in); got != tc.want {
			t.Errorf("shortDigest(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
