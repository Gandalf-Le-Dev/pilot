package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFleet(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["fleet.yaml"] = files["fleet.yaml"] + "\nhosts:\n  web:\n    address: web.example.com\n"
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const composeSvc = "runtime: compose\nhosts: [web]\ncompose:\n  file: compose.yaml\n"

// TestServiceDirectoryImpliesItsOwnSource is the point of the layout: the
// definition sits beside what it deploys, so the `source: {path: …}` line that
// only ever pointed across a gap disappears.
func TestServiceDirectoryImpliesItsOwnSource(t *testing.T) {
	root := writeFleet(t, map[string]string{
		"fleet.yaml":                   "version: 1\n",
		"services/wakapi/service.yaml": composeSvc,
		"services/wakapi/compose.yaml": "services: {}\n",
	})

	f, ds, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}

	s, ok := f.Services["wakapi"]
	if !ok {
		t.Fatalf("service not found; got %v", f.Services)
	}
	if s.Source == nil || s.Source.Path != "services/wakapi" {
		t.Errorf("source = %+v, want the service's own directory", s.Source)
	}
	if s.File != "services/wakapi/service.yaml" {
		t.Errorf("File = %q, want the path a diagnostic can point at", s.File)
	}
}

// TestExplicitSourceWins — a service built from a git repo must not have its
// source silently replaced by the directory it happens to be described in.
func TestExplicitSourceWins(t *testing.T) {
	root := writeFleet(t, map[string]string{
		"fleet.yaml":                 "version: 1\n",
		"services/kite/service.yaml": composeSvc + "source:\n  repo: https://example.com/kite.git\n  ref: main\n",
		"services/kite/compose.yaml": "services: {}\n",
	})

	f, _, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if src := f.Services["kite"].Source; src != nil && src.Path != "" {
		t.Errorf("source.path = %q, want it left alone when a repo is given", src.Path)
	}
}

// TestFlatLayoutStillWorks — an existing fleet must not break to gain this.
func TestFlatLayoutStillWorks(t *testing.T) {
	root := writeFleet(t, map[string]string{
		"fleet.yaml":              "version: 1\n",
		"services/wakapi.yaml":    composeSvc + "source:\n  path: src/wakapi\n",
		"src/wakapi/compose.yaml": "services: {}\n",
	})

	f, ds, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if ds.HasErrors() {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	if src := f.Services["wakapi"].Source; src == nil || src.Path != "src/wakapi" {
		t.Errorf("source = %+v, want path src/wakapi", f.Services["wakapi"].Source)
	}
}

func TestBothLayoutsCanCoexist(t *testing.T) {
	root := writeFleet(t, map[string]string{
		"fleet.yaml":                "version: 1\n",
		"services/old.yaml":         composeSvc,
		"services/new/service.yaml": composeSvc,
		"services/new/compose.yaml": "services: {}\n",
	})

	f, _, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "new"} {
		if _, ok := f.Services[name]; !ok {
			t.Errorf("service %q was not loaded; got %v", name, f.Services)
		}
	}
}

// TestDirectoryWithoutDefinitionIsReported is the failure worth being loud
// about. A half-migrated service — compose file moved, definition not — would
// otherwise vanish, and the first sign would be `pilot status` listing one
// fewer service than expected.
func TestDirectoryWithoutDefinitionIsReported(t *testing.T) {
	root := writeFleet(t, map[string]string{
		"fleet.yaml":                   "version: 1\n",
		"services/wakapi/compose.yaml": "services: {}\n",
	})

	_, ds, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !ds.HasErrors() {
		t.Fatal("a services/ directory with no service.yaml must be reported, not skipped")
	}

	var found bool
	for _, d := range ds.Sorted() {
		if strings.Contains(d.File, "services/wakapi") && strings.Contains(d.Message, ServiceFile) {
			found = true
			if d.Hint == "" {
				t.Error("the diagnostic should say what to do about it")
			}
		}
	}
	if !found {
		t.Errorf("no diagnostic named the directory: %v", ds.Sorted())
	}
}

// TestNameMismatchNamesTheDirectory — the flat form says "rename the file", so
// the directory form must say "rename the directory" or the advice is wrong.
func TestNameMismatchNamesTheDirectory(t *testing.T) {
	root := writeFleet(t, map[string]string{
		"fleet.yaml":                   "version: 1\n",
		"services/wakapi/service.yaml": "name: something-else\n" + composeSvc,
		"services/wakapi/compose.yaml": "services: {}\n",
	})

	_, ds, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds.Sorted() {
		if d.Field == "name" {
			if !strings.Contains(d.Hint, "directory") {
				t.Errorf("hint should name the directory, got %q", d.Hint)
			}
			return
		}
	}
	t.Error("no name-mismatch diagnostic")
}
