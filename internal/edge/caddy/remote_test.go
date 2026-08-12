package caddy

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport/ssh"
)

// fakeRunner emulates a host: an in-memory filesystem plus scripted responses
// for the caddy binary.
type fakeRunner struct {
	files map[string]string
	cmds  []string

	validateFails bool
	reloadFails   bool
	writeFails    bool
	corruptWrite  bool
	missing       map[string]bool
}

func newFake(files map[string]string) *fakeRunner {
	f := &fakeRunner{files: map[string]string{}, missing: map[string]bool{}}
	maps.Copy(f.files, files)
	return f
}

func (f *fakeRunner) Run(ctx context.Context, cmd string) (ssh.Result, error) {
	f.cmds = append(f.cmds, cmd)
	fields := strings.Fields(cmd)

	switch {
	case strings.HasPrefix(cmd, "caddy validate"):
		if f.validateFails {
			return ssh.Result{ExitCode: 1, Stderr: "adapting config: unexpected token"}, nil
		}
	case strings.HasPrefix(cmd, "caddy reload"):
		if f.reloadFails {
			return ssh.Result{ExitCode: 1, Stderr: "loading config: refused"}, nil
		}
	case strings.HasPrefix(cmd, "cp "):
		src, dst := unquote(fields[len(fields)-2]), unquote(fields[len(fields)-1])
		if v, ok := f.files[src]; ok {
			f.files[dst] = v
		}
	case strings.HasPrefix(cmd, "rm -f "):
		delete(f.files, unquote(fields[len(fields)-1]))
	case strings.HasPrefix(cmd, "test -e "):
		if _, ok := f.files[unquote(fields[len(fields)-1])]; !ok {
			return ssh.Result{ExitCode: 1}, nil
		}
	case strings.HasPrefix(cmd, "ls -1t "):
		prefix := strings.TrimSuffix(unquote(strings.Fields(strings.TrimPrefix(cmd, "ls -1t "))[0]), "*")
		var found []string
		for p := range f.files {
			if strings.HasPrefix(p, prefix) {
				found = append(found, p)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(found)))
		return ssh.Result{Stdout: strings.Join(found, "\n")}, nil
	case strings.HasPrefix(cmd, "ls -1 "):
		var names []string
		dir := unquote(strings.Fields(strings.TrimPrefix(cmd, "ls -1 "))[0])
		for p := range f.files {
			if base, ok := strings.CutPrefix(p, dir+"/"); ok {
				names = append(names, base)
			}
		}
		return ssh.Result{Stdout: strings.Join(names, "\n")}, nil
	}
	return ssh.Result{}, nil
}

func unquote(s string) string {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "'"), "'")
	return strings.ReplaceAll(s, `'\''`, "'")
}

func (f *fakeRunner) RunScript(ctx context.Context, body string) (ssh.Result, error) {
	return f.Run(ctx, body)
}

func (f *fakeRunner) ReadFile(ctx context.Context, p string) ([]byte, error) {
	v, ok := f.files[p]
	if !ok {
		return nil, &notFound{p}
	}
	return []byte(v), nil
}

func (f *fakeRunner) WriteFile(ctx context.Context, p string, data []byte, mode string) error {
	switch {
	case f.writeFails:
		// A real shell can truncate and *then* fail, which is exactly how the
		// Caddyfile was lost.
		f.files[p] = ""
		return fmt.Errorf("simulated write failure after truncation")
	case f.corruptWrite:
		f.corruptWrite = false
		f.files[p] = "partially written garbage"
		return nil
	}
	f.files[p] = string(data)
	return nil
}

type notFound struct{ path string }

func (e *notFound) Error() string { return e.path + ": no such file" }

func (f *fakeRunner) ran(substr string) bool {
	for _, c := range f.cmds {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

var testPaths = Paths{
	Caddyfile:  "/etc/caddy/Caddyfile",
	SnippetDir: "/etc/caddy/pilot.d",
	Admin:      "http://127.0.0.1:2019",
}

var at = time.Date(2026, 7, 27, 10, 30, 0, 0, time.UTC)

func TestEnsureImportAppendsAndReloads(t *testing.T) {
	f := newFake(map[string]string{
		"/etc/caddy/Caddyfile": "example.com {\n\tfile_server\n}\n",
	})

	action, err := EnsureImport(context.Background(), f, testPaths, at)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionInstalled {
		t.Errorf("action = %q, want %q", action, ActionInstalled)
	}
	if !strings.Contains(f.files["/etc/caddy/Caddyfile"], "import pilot.d/*.caddy") {
		t.Errorf("import not added:\n%s", f.files["/etc/caddy/Caddyfile"])
	}

	// A backup was taken before the edit, and it holds the original.
	backup := BackupName(testPaths.Caddyfile, at)
	if got := f.files[backup]; !strings.Contains(got, "file_server") || strings.Contains(got, "import pilot.d") {
		t.Errorf("backup should hold the pre-edit content, got:\n%s", got)
	}
	if !f.ran("caddy validate") || !f.ran("caddy reload") {
		t.Errorf("expected validate then reload, ran: %v", f.cmds)
	}
}

func TestEnsureImportIsIdempotent(t *testing.T) {
	f := newFake(map[string]string{
		"/etc/caddy/Caddyfile": "example.com {\n\tfile_server\n}\n\nimport pilot.d/*.caddy\n",
	})

	action, err := EnsureImport(context.Background(), f, testPaths, at)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionNone {
		t.Errorf("action = %q, want %q", action, ActionNone)
	}
	if len(f.cmds) != 0 {
		t.Errorf("an already-configured host should not be touched, ran: %v", f.cmds)
	}
}

// If Caddy rejects the edited file, the original must come back automatically.
func TestEnsureImportRestoresBackupOnValidationFailure(t *testing.T) {
	original := "example.com {\n\tfile_server\n}\n"
	f := newFake(map[string]string{"/etc/caddy/Caddyfile": original})
	f.validateFails = true

	_, err := EnsureImport(context.Background(), f, testPaths, at)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Errorf("error should say the file was restored: %v", err)
	}
	if got := f.files["/etc/caddy/Caddyfile"]; got != original {
		t.Errorf("Caddyfile not restored:\n%s", got)
	}
	if f.ran("caddy reload") {
		t.Error("must not reload after a failed validation")
	}
}

func TestEnsureImportRefusesBraceLessCaddyfile(t *testing.T) {
	f := newFake(map[string]string{
		"/etc/caddy/Caddyfile": "example.com\nroot * /var/www\nfile_server\n",
	})

	_, err := EnsureImport(context.Background(), f, testPaths, at)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "wrap the existing site in braces") {
		t.Errorf("error should carry the remedy: %v", err)
	}
	if len(f.cmds) != 0 {
		t.Errorf("a refusal must not touch the host, ran: %v", f.cmds)
	}
}

// A release that changes only application code renders an identical snippet;
// reloading Caddy for that would be pure noise.
func TestInstallSnippetSkipsReloadWhenUnchanged(t *testing.T) {
	const snippet = "api.example.com {\n\treverse_proxy 127.0.0.1:8080\n}\n"
	f := newFake(map[string]string{
		"/etc/caddy/Caddyfile":         "import pilot.d/*.caddy\n",
		"/etc/caddy/pilot.d/api.caddy": snippet,
	})

	action, err := InstallSnippet(context.Background(), f, testPaths, "api", snippet)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionNone {
		t.Errorf("action = %q, want %q", action, ActionNone)
	}
	if f.ran("caddy reload") {
		t.Error("an unchanged route must not trigger a reload")
	}
}

func TestInstallSnippetWritesAndReloads(t *testing.T) {
	f := newFake(map[string]string{"/etc/caddy/Caddyfile": "import pilot.d/*.caddy\n"})

	action, err := InstallSnippet(context.Background(), f, testPaths, "api", "api.example.com {\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionInstalled {
		t.Errorf("action = %q, want %q", action, ActionInstalled)
	}
	if f.files["/etc/caddy/pilot.d/api.caddy"] == "" {
		t.Error("snippet not written")
	}
	if !f.ran("caddy reload") {
		t.Error("a new route should reload")
	}
}

// A rejected route must not be left on disk, or Caddy would fail to start on
// its next restart — turning a bad deploy into a later outage.
func TestInstallSnippetRollsBackOnValidationFailure(t *testing.T) {
	t.Run("new route is removed", func(t *testing.T) {
		f := newFake(map[string]string{"/etc/caddy/Caddyfile": "import pilot.d/*.caddy\n"})
		f.validateFails = true

		if _, err := InstallSnippet(context.Background(), f, testPaths, "api", "bad {{{\n"); err == nil {
			t.Fatal("want an error")
		}
		if _, exists := f.files["/etc/caddy/pilot.d/api.caddy"]; exists {
			t.Error("invalid route should have been removed")
		}
	})

	t.Run("existing route is restored", func(t *testing.T) {
		const good = "api.example.com {\n\treverse_proxy 127.0.0.1:8080\n}\n"
		f := newFake(map[string]string{
			"/etc/caddy/Caddyfile":         "import pilot.d/*.caddy\n",
			"/etc/caddy/pilot.d/api.caddy": good,
		})
		f.validateFails = true

		if _, err := InstallSnippet(context.Background(), f, testPaths, "api", "bad {{{\n"); err == nil {
			t.Fatal("want an error")
		}
		if got := f.files["/etc/caddy/pilot.d/api.caddy"]; got != good {
			t.Errorf("previous route not restored:\n%s", got)
		}
	})
}

func TestRemoveSnippet(t *testing.T) {
	f := newFake(map[string]string{
		"/etc/caddy/Caddyfile":            "import pilot.d/*.caddy\n",
		"/etc/caddy/pilot.d/oldapp.caddy": "oldapp.example.com {\n}\n",
	})

	action, err := RemoveSnippet(context.Background(), f, testPaths, "oldapp")
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionRemoved {
		t.Errorf("action = %q, want %q", action, ActionRemoved)
	}
	if _, exists := f.files["/etc/caddy/pilot.d/oldapp.caddy"]; exists {
		t.Error("snippet not removed")
	}

	// Removing something absent is a no-op, not an error.
	action, err = RemoveSnippet(context.Background(), f, testPaths, "neverexisted")
	if err != nil || action != ActionNone {
		t.Errorf("got %q, %v", action, err)
	}
}

func TestListSnippetsAndOrphans(t *testing.T) {
	f := newFake(map[string]string{
		"/etc/caddy/pilot.d/api.caddy":    "x",
		"/etc/caddy/pilot.d/blog.caddy":   "x",
		"/etc/caddy/pilot.d/oldapp.caddy": "x",
	})

	got, err := ListSnippets(context.Background(), f, testPaths)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "api,blog,oldapp" {
		t.Errorf("ListSnippets = %v", got)
	}

	orphans := Orphans(got, map[string]bool{"api": true, "blog": true})
	if strings.Join(orphans, ",") != "oldapp" {
		t.Errorf("Orphans = %v, want [oldapp]", orphans)
	}
}

func TestReloadFailureIsReportedAsNonFatalToServing(t *testing.T) {
	f := newFake(map[string]string{"/etc/caddy/Caddyfile": "import pilot.d/*.caddy\n"})
	f.reloadFails = true

	err := Reload(context.Background(), f, testPaths)
	if err == nil {
		t.Fatal("want an error")
	}
	// Operators need to know the site is still up, not panic about an outage.
	if !strings.Contains(err.Error(), "previous config still serving") {
		t.Errorf("error should reassure that serving continues: %v", err)
	}
}

func TestBackupName(t *testing.T) {
	got := BackupName("/etc/caddy/Caddyfile", at)
	if got != "/etc/caddy/Caddyfile.pilot-bak-20260727T103000Z" {
		t.Errorf("BackupName = %q", got)
	}
}

// The incident this guards against, reproduced.
//
// A write that fails *after* truncating used to be treated as "nothing
// happened": EnsureImport restored only when validation failed, so the
// wrecked file survived with a backup nobody knew to use.
func TestEnsureImportRestoresWhenTheWriteFails(t *testing.T) {
	original := "example.com {\n\tfile_server\n}\n\nnotes.example.com {\n\treverse_proxy localhost:3001\n}\n"
	f := newFake(map[string]string{"/etc/caddy/Caddyfile": original})
	f.writeFails = true

	_, err := EnsureImport(context.Background(), f, testPaths, at)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Errorf("error should say the file was restored: %v", err)
	}
	if got := f.files["/etc/caddy/Caddyfile"]; got != original {
		t.Errorf("Caddyfile not restored after a failed write:\n%q", got)
	}
}

// A write can also "succeed" having landed something else. Reading it back is
// the only way to know.
func TestEnsureImportRestoresWhenTheWriteLandsWrong(t *testing.T) {
	original := "example.com {\n\tfile_server\n}\n"
	f := newFake(map[string]string{"/etc/caddy/Caddyfile": original})
	f.corruptWrite = true

	_, err := EnsureImport(context.Background(), f, testPaths, at)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "does not match what was written") {
		t.Errorf("error should name the mismatch: %v", err)
	}
	if got := f.files["/etc/caddy/Caddyfile"]; got != original {
		t.Errorf("Caddyfile not restored:\n%q", got)
	}
}

// The second half: a retry must not build on wreckage. This is what actually
// took the sites down — a second --fix appended an import to a file that had
// already lost most of its content, then reloaded Caddy with it.
func TestEnsureImportRefusesATruncatedCaddyfile(t *testing.T) {
	full := strings.Repeat("example.com {\n\tfile_server\n}\n\n", 20)
	f := newFake(map[string]string{
		"/etc/caddy/Caddyfile":                            "# almost nothing left\n",
		"/etc/caddy/Caddyfile.pilot-bak-20260728T081636Z": full,
	})

	_, err := EnsureImport(context.Background(), f, testPaths, at)
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"looks like a previous run damaged it", "diff", "cp -p"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q:\n%v", want, err)
		}
	}
	if f.ran("caddy reload") {
		t.Error("must not reload a config it refused to trust")
	}
}

// Deleting a site or two is normal editing, not damage.
func TestEnsureImportAllowsModestShrinkage(t *testing.T) {
	full := strings.Repeat("example.com {\n\tfile_server\n}\n\n", 20)
	slightly := strings.Repeat("example.com {\n\tfile_server\n}\n\n", 15)

	f := newFake(map[string]string{
		"/etc/caddy/Caddyfile":                            slightly,
		"/etc/caddy/Caddyfile.pilot-bak-20260728T081636Z": full,
	})

	if _, err := EnsureImport(context.Background(), f, testPaths, at); err != nil {
		t.Fatalf("a legitimately edited file should still be modifiable: %v", err)
	}
}

func TestInstallSnippetUndoesAFailedWrite(t *testing.T) {
	const good = "api.example.com {\n\treverse_proxy 127.0.0.1:8080\n}\n"
	f := newFake(map[string]string{
		"/etc/caddy/Caddyfile":         "import pilot.d/*.caddy\n",
		"/etc/caddy/pilot.d/api.caddy": good,
	})
	f.corruptWrite = true

	if _, err := InstallSnippet(context.Background(), f, testPaths, "api", "api.example.com {\n}\n"); err == nil {
		t.Fatal("want an error")
	}
	if got := f.files["/etc/caddy/pilot.d/api.caddy"]; got != good {
		t.Errorf("previous route not restored:\n%q", got)
	}
}

// A bind-less route on a host whose Caddyfile binds explicitly would install,
// validate, obtain a certificate, and serve an empty 200 forever — refusing at
// install time is the only reliable moment to catch it.
func TestInstallSnippetRefusesBindlessRouteOnBindingHost(t *testing.T) {
	bindingCaddyfile := "box.example.com {\n\tbind 37.187.24.219\n}\nimport pilot.d/*.caddy\n"
	fake := newFake(map[string]string{testPaths.Caddyfile: bindingCaddyfile})

	_, err := InstallSnippet(context.Background(), fake, testPaths, "docs",
		"docs.example.com {\n\tfile_server\n}\n")
	if err == nil || !strings.Contains(err.Error(), "caddy.bind") {
		t.Fatalf("want a refusal naming the fix, got: %v", err)
	}
	if _, ok := fake.files["/etc/caddy/pilot.d/docs.caddy"]; ok {
		t.Error("the dead route must not be installed")
	}

	// A route carrying its own bind joins the same server and is fine.
	if _, err := InstallSnippet(context.Background(), fake, testPaths, "docs",
		"docs.example.com {\n\tbind 37.187.24.219\n\tfile_server\n}\n"); err != nil {
		t.Fatalf("a bound route should install: %v", err)
	}
}

// default_bind in the global options applies to generated sites too, so it
// closes the gap without any per-route bind.
func TestInstallSnippetAcceptsBindlessRouteUnderDefaultBind(t *testing.T) {
	fake := newFake(map[string]string{
		testPaths.Caddyfile: "{\n\tdefault_bind 37.187.24.219\n}\nbox.example.com {\n\tbind 37.187.24.219\n}\n",
	})
	if _, err := InstallSnippet(context.Background(), fake, testPaths, "docs",
		"docs.example.com {\n\tfile_server\n}\n"); err != nil {
		t.Fatalf("default_bind should make a bind-less route safe: %v", err)
	}
}
