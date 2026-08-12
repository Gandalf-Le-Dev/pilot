package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ctx() context.Context { return context.Background() }

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	e := New("test-host")

	res, err := e.Run(ctx(), "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || res.Out() != "hello" {
		t.Errorf("res = %+v", res)
	}

	// A non-zero exit is data, not a transport failure — the same contract the
	// SSH executor honours.
	res, err = e.Run(ctx(), "exit 3")
	if err != nil {
		t.Fatalf("a failing command should not be an error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if res.Err() == nil {
		t.Error("Result.Err should report the failure for callers that need success")
	}
}

func TestRunSupportsPipesAndRedirection(t *testing.T) {
	e := New("")
	res, err := e.Run(ctx(), "printf 'b\\na\\n' | sort | tr -d '\\n'")
	if err != nil {
		t.Fatal(err)
	}
	if res.Out() != "ab" {
		t.Errorf("got %q; command lines with pipes must work, since that is what callers send", res.Out())
	}
}

func TestRunScriptAbortsOnFirstFailure(t *testing.T) {
	e := New("")
	marker := filepath.Join(t.TempDir(), "should-not-exist")

	res, err := e.RunScript(ctx(), "false\ntouch "+marker)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Error("script should have failed at the first command")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("execution continued past a failing command; set -e is not in effect")
	}
}

func TestWriteFilePermissionsAndAtomicity(t *testing.T) {
	e := New("")
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", ".env")

	if err := e.WriteFile(ctx(), path, []byte("SECRET=1\n"), "0600"); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600 for a file holding secrets", perm)
	}

	if err := e.WriteFile(ctx(), path, []byte("SECRET=2\n"), "0600"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want just the target — a temp file leaked", len(entries))
	}

	got, err := e.ReadFile(ctx(), path)
	if err != nil || string(got) != "SECRET=2\n" {
		t.Errorf("read back %q, %v", got, err)
	}
}

func TestWriteFileRejectsBadMode(t *testing.T) {
	e := New("")
	path := filepath.Join(t.TempDir(), "f")
	if err := e.WriteFile(ctx(), path, []byte("x"), "not-a-mode"); err == nil {
		t.Error("want an error for an unparseable mode")
	}
}

func TestExistsAndHasCommand(t *testing.T) {
	e := New("")
	dir := t.TempDir()

	ok, err := e.Exists(ctx(), dir)
	if err != nil || !ok {
		t.Errorf("Exists(dir) = %v, %v", ok, err)
	}
	ok, err = e.Exists(ctx(), filepath.Join(dir, "nope"))
	if err != nil || ok {
		t.Errorf("Exists(missing) = %v, %v", ok, err)
	}

	// A dangling symlink still exists as an entry, which is what callers checking
	// for `current` need to know.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.Exists(ctx(), link); !ok {
		t.Error("a dangling symlink should report as existing")
	}

	if ok, _ := e.HasCommand(ctx(), "sh"); !ok {
		t.Error("sh should be found")
	}
	if ok, _ := e.HasCommand(ctx(), "definitely-not-a-real-command-xyz"); ok {
		t.Error("a missing command should report false")
	}
}

// The agent runs as root, so a path assembled from an empty variable must never
// expand into something catastrophic.
func TestRemoveAllRefusesDangerousPaths(t *testing.T) {
	e := New("")
	for _, bad := range []string{"", "/", ".", "relative/path", "/opt", "/etc"} {
		if err := e.RemoveAll(ctx(), bad); err == nil {
			t.Errorf("RemoveAll(%q) should have been refused", bad)
		}
	}

	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveAll(ctx(), deep); err != nil {
		t.Errorf("a legitimate deep path should be removable: %v", err)
	}
	if _, err := os.Stat(deep); err == nil {
		t.Error("path was not removed")
	}
}

func TestUploadDirCopiesTreePreservingSymlinks(t *testing.T) {
	e := New("")
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "target")

	if err := os.MkdirAll(filepath.Join(src, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.html", filepath.Join(src, "home.html")); err != nil {
		t.Fatal(err)
	}

	if err := e.UploadDir(ctx(), src, dst); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "assets", "app.js"))
	if err != nil || string(got) != "console.log(1)" {
		t.Errorf("nested file = %q, %v", got, err)
	}

	// Preserved rather than dereferenced, matching the SSH executor's tar path.
	target, err := os.Readlink(filepath.Join(dst, "home.html"))
	if err != nil || target != "index.html" {
		t.Errorf("symlink = %q, %v", target, err)
	}
}

// Staging permissions describe the builder's machine, not the release: a
// 0700 directory straight from os.MkdirTemp would be unreadable by the web
// server that has to serve it.
func TestUploadDirNormalizesModes(t *testing.T) {
	e := New("")
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "target")

	if err := os.MkdirAll(filepath.Join(src, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "private", "index.html"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := e.UploadDir(ctx(), src, dst); err != nil {
		t.Fatal(err)
	}

	want := map[string]os.FileMode{
		".":                  0o755,
		"private":            0o755,
		"private/index.html": 0o644,
		"run.sh":             0o755, // execute bits survive for staged binaries
	}
	for rel, mode := range want {
		fi, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != mode {
			t.Errorf("%s mode = %o, want %o", rel, got, mode)
		}
	}
}

func TestUploadDirRejectsNonDirectory(t *testing.T) {
	e := New("")
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.UploadDir(ctx(), f, t.TempDir()); err == nil {
		t.Error("want an error when the source is not a directory")
	}
}

func TestStreamForwardsOutput(t *testing.T) {
	e := New("")
	var out strings.Builder

	code, err := e.Stream(ctx(), "echo streamed", &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !strings.Contains(out.String(), "streamed") {
		t.Errorf("code = %d, out = %q", code, out.String())
	}
}

func TestLabelFallsBackToHostname(t *testing.T) {
	if got := New("web-1").Label(); got != "web-1" {
		t.Errorf("Label = %q", got)
	}
	if got := (&Executor{}).Label(); got != "localhost" {
		t.Errorf("empty executor Label = %q, want localhost", got)
	}
}
