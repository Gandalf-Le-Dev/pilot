package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

// TestServiceYamlIsNotShipped covers the change that makes the per-service
// directory layout adoptable without redeploying the fleet.
//
// The definition sits beside the compose file it deploys, so it lands inside the
// build output. Two things must not happen: the host must not receive Pilot's own
// configuration, and — the load-bearing one — the release digest must not change,
// or moving files around would force a redeploy of every service.
func TestServiceYamlIsNotShipped(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.yaml", "services:\n  app:\n    image: nginx\n")

	// Digest with only the deployable content present.
	before, _, err := hashPath(dir)
	if err != nil {
		t.Fatal(err)
	}

	write(config.ServiceFile, "runtime: compose\nhosts: [web]\n")

	after, _, err := hashPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("adding %s changed the release digest (%s → %s);\n"+
			"every service would need a redeploy purely to move files",
			config.ServiceFile, before, after)
	}

	stage, _, err := collect(dir, []string{"./"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)

	if _, err := os.Stat(filepath.Join(stage, config.ServiceFile)); !os.IsNotExist(err) {
		t.Errorf("%s was staged for the host; it is Pilot's config, not the service's", config.ServiceFile)
	}
	if _, err := os.Stat(filepath.Join(stage, "compose.yaml")); err != nil {
		t.Errorf("the deployable content was not staged: %v", err)
	}
}
