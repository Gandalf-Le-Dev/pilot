package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

func imageEnv(t *testing.T, compose string, manage config.Manage) *Env {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "services", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	return &Env{Fleet: &config.Fleet{
		Root: root,
		Services: map[string]*config.Service{"app": {
			Name:    "app",
			Manage:  manage,
			Compose: &config.Compose{File: "compose.yaml"},
			Source:  &config.Source{Path: "services/app"},
		}},
	}}
}

// TestUnpinnedImageIsReported covers the failure that prompted this check: a
// service on `:latest` whose rollback silently does nothing.
func TestUnpinnedImageIsReported(t *testing.T) {
	env := imageEnv(t, "services:\n  app:\n    image: ghcr.io/me/app:latest\n", config.ManageDeploy)

	found := checkImageTags(context.Background(), env)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1", len(found))
	}
	f := found[0]

	if f.Status != StatusWarn {
		t.Errorf("status = %v, want warn", f.Status)
	}
	// Both consequences must be stated. "Unpinned image" alone reads as style
	// advice; the point is that two things are already broken.
	for _, want := range []string{"nothing to do", "Rollback"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail should explain %q:\n%s", want, f.Detail)
		}
	}
	if !strings.Contains(f.Hint, "ghcr.io/me/app:") {
		t.Errorf("hint should suggest pinning this image, got %q", f.Hint)
	}
}

// TestImplicitLatestIsReported — `image: nginx` is `nginx:latest`, and the
// absence of a tag makes it easier to miss, not less broken.
func TestImplicitLatestIsReported(t *testing.T) {
	env := imageEnv(t, "services:\n  app:\n    image: nginx\n", config.ManageDeploy)
	if got := checkImageTags(context.Background(), env); len(got) != 1 {
		t.Fatalf("got %+v, want a finding for an untagged image", got)
	}
}

func TestPinnedImagesAreQuiet(t *testing.T) {
	for _, compose := range []string{
		"services:\n  app:\n    image: ghcr.io/atuinsh/atuin:v18.12.0\n",
		"services:\n  app:\n    image: postgres:16-alpine\n",
		"services:\n  app:\n    image: nginx@sha256:0000000000000000000000000000000000000000000000000000000000000000\n",
	} {
		env := imageEnv(t, compose, config.ManageDeploy)
		if got := checkImageTags(context.Background(), env); len(got) != 0 {
			t.Errorf("false positive on %q: %+v", strings.TrimSpace(compose), got)
		}
	}
}

// TestRegistryPortIsNotATag guards the one parsing trap: the colon in
// `registry:5000/app` belongs to a host:port, not a tag, so that image is
// untagged and therefore latest — but `5000/app` must never be read as the tag.
func TestRegistryPortIsNotATag(t *testing.T) {
	env := imageEnv(t, "services:\n  app:\n    image: registry.example.com:5000/app\n", config.ManageDeploy)

	// A finding at all proves the tag logic did not read `5000/app` as the tag —
	// had it done so, the tag would not be "latest" and this would be silent.
	found := checkImageTags(context.Background(), env)
	if len(found) != 1 {
		t.Fatalf("got %+v, want it treated as untagged", found)
	}

	// And the suggestion must keep the port, since dropping it would name an
	// image that does not exist.
	if !strings.Contains(found[0].Hint, "registry.example.com:5000/app:") {
		t.Errorf("the suggested fix lost the registry port: %q", found[0].Hint)
	}
}

// TestInterpolatedImageIsSkipped — a `${VAR}` reference cannot be judged from
// the file, and guessing would produce a warning nobody can act on.
func TestInterpolatedImageIsSkipped(t *testing.T) {
	env := imageEnv(t, "services:\n  app:\n    image: ghcr.io/me/app:${TAG}\n", config.ManageDeploy)
	if got := checkImageTags(context.Background(), env); len(got) != 0 {
		t.Errorf("got %+v, want silence on an interpolated tag", got)
	}
}

// TestObserveServicesAreSkipped — Pilot never deploys them, so neither
// consequence applies.
func TestObserveServicesAreSkipped(t *testing.T) {
	env := imageEnv(t, "services:\n  app:\n    image: postgres:latest\n", config.ManageObserve)
	if got := checkImageTags(context.Background(), env); len(got) != 0 {
		t.Errorf("got %+v, want silence for manage: observe", got)
	}
}

// TestOneFindingPerService keeps a multi-container stack from producing a wall
// of near-identical warnings.
func TestOneFindingPerService(t *testing.T) {
	env := imageEnv(t, "services:\n  a:\n    image: one:latest\n  b:\n    image: two:latest\n", config.ManageDeploy)

	found := checkImageTags(context.Background(), env)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want them grouped into 1", len(found))
	}
	for _, want := range []string{"one:latest", "two:latest"} {
		if !strings.Contains(found[0].Title, want) {
			t.Errorf("title should name %q: %q", want, found[0].Title)
		}
	}
}
