package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

func bgEnv(t *testing.T, compose string, rollout *config.Rollout) *Env {
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
			Manage:  config.ManageDeploy,
			Compose: &config.Compose{File: "compose.yaml"},
			Source:  &config.Source{Path: "services/app"},
			Rollout: rollout,
		}},
	}}
}

func blueGreen() *config.Rollout {
	return &config.Rollout{Strategy: "blue-green", Service: "app", Ports: []int{18080, 18081}}
}

// TestNamedVolumeWithBlueGreenIsReported is the data-loss case. Compose scopes a
// named volume to its project and each colour is a separate project, so the new
// colour comes up with an empty volume and the deploy still reports success.
func TestNamedVolumeWithBlueGreenIsReported(t *testing.T) {
	compose := "services:\n  app:\n    image: app:1.0\n    volumes:\n      - app-data:/data\nvolumes:\n  app-data:\n"

	found := checkBlueGreen(context.Background(), bgEnv(t, compose, blueGreen()))
	if len(found) != 1 {
		t.Fatalf("got %+v, want one finding", found)
	}
	if found[0].Status != StatusFail {
		t.Errorf("status = %v; silent data loss is not a warning", found[0].Status)
	}
	if !strings.Contains(found[0].Detail, "empty volume") {
		t.Errorf("detail should say what actually happens: %q", found[0].Detail)
	}
}

// TestExtraPublishedPortIsReported covers the failure that stopped the real
// deploy: both colours run at once, so any port Pilot does not assign is held by
// the colour already running.
func TestExtraPublishedPortIsReported(t *testing.T) {
	compose := "services:\n  app:\n    image: app:1.0\n    ports:\n      - \"100.64.0.1:8081:8081\"\n"

	found := checkBlueGreen(context.Background(), bgEnv(t, compose, blueGreen()))
	if len(found) != 1 {
		t.Fatalf("got %+v, want one finding", found)
	}
	if !strings.Contains(found[0].Title, "8081") {
		t.Errorf("title should name the contended port: %q", found[0].Title)
	}
}

// TestRolloutOwnPortsAreNotReported — Pilot assigns those, so they are the one
// case that is fine.
func TestRolloutOwnPortsAreNotReported(t *testing.T) {
	compose := "services:\n  app:\n    image: app:1.0\n    ports:\n      - \"127.0.0.1:18080:8080\"\n"
	if got := checkBlueGreen(context.Background(), bgEnv(t, compose, blueGreen())); len(got) != 0 {
		t.Errorf("got %+v, want silence for a port the rollout owns", got)
	}
}

// TestRecreateServicesAreSkipped — neither problem exists without two colours.
func TestRecreateServicesAreSkipped(t *testing.T) {
	compose := "services:\n  app:\n    image: app:1.0\n    ports:\n      - \"8081:8081\"\nvolumes:\n  app-data:\n"
	r := &config.Rollout{Strategy: "recreate"}
	if got := checkBlueGreen(context.Background(), bgEnv(t, compose, r)); len(got) != 0 {
		t.Errorf("got %+v, want silence for recreate", got)
	}
	if got := checkBlueGreen(context.Background(), bgEnv(t, compose, nil)); len(got) != 0 {
		t.Errorf("got %+v, want silence when no rollout is configured", got)
	}
}

// TestBindMountIsFine — a host path is shared between colours, which is the
// recommended fix, so it must not be reported.
func TestBindMountIsFine(t *testing.T) {
	compose := "services:\n  app:\n    image: app:1.0\n    volumes:\n      - ./data:/data\n"
	if got := checkBlueGreen(context.Background(), bgEnv(t, compose, blueGreen())); len(got) != 0 {
		t.Errorf("got %+v, want silence for a bind mount", got)
	}
}

// TestInterpolatedHostAddressIsMatched is a regression test for a miss found by
// running the check against the config that had actually failed.
//
// The first regex accepted only a numeric host address, so `${BIND_TS}:8081:8081`
// — the exact line that stopped a real blue-green deploy — went unreported. A
// check that misses the case that motivated it is worse than no check, because
// it reads as a clean bill of health.
func TestInterpolatedHostAddressIsMatched(t *testing.T) {
	compose := "services:\n  app:\n    image: app:1.0\n    ports:\n" +
		"      - \"${BIND_LOCAL}:8085:8080\"\n      - \"${BIND_TS}:8081:8081\"\n"

	found := checkBlueGreen(context.Background(), bgEnv(t, compose, blueGreen()))
	if len(found) != 1 {
		t.Fatalf("got %+v, want one finding", found)
	}
	for _, want := range []string{"8081", "8085"} {
		if !strings.Contains(found[0].Title, want) {
			t.Errorf("title should name port %s: %q", want, found[0].Title)
		}
	}
}
