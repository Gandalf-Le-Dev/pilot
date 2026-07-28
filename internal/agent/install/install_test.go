package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// fakeHost records what would have been run and written.
type fakeHost struct {
	cmds  []string
	files map[string][]byte

	uname     string
	noSystemd bool
	failStart bool
}

func newFake(uname string) *fakeHost {
	return &fakeHost{uname: uname, files: map[string][]byte{}}
}

func (f *fakeHost) Run(ctx context.Context, cmd string) (transport.Result, error) {
	f.cmds = append(f.cmds, cmd)
	switch {
	case strings.HasPrefix(cmd, "uname"):
		return transport.Result{Stdout: f.uname + "\n"}, nil
	case strings.Contains(cmd, "systemctl"):
		if f.failStart {
			return transport.Result{ExitCode: 1, Stderr: "Job for pilotd.service failed"}, nil
		}
	}
	return transport.Result{}, nil
}

func (f *fakeHost) RunScript(ctx context.Context, body string) (transport.Result, error) {
	return f.Run(ctx, body)
}

func (f *fakeHost) RunInput(ctx context.Context, cmd string, _ []byte) (transport.Result, error) {
	return f.Run(ctx, cmd)
}

func (f *fakeHost) Stream(ctx context.Context, cmd string, _, _ io.Writer) (int, error) {
	res, _ := f.Run(ctx, cmd)
	return res.ExitCode, nil
}

func (f *fakeHost) ReadFile(ctx context.Context, p string) ([]byte, error) {
	b, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (f *fakeHost) WriteFile(ctx context.Context, p string, data []byte, mode string) error {
	f.files[p] = append([]byte(nil), data...)
	return nil
}

func (f *fakeHost) Exists(ctx context.Context, p string) (bool, error) {
	_, ok := f.files[p]
	return ok, nil
}

func (f *fakeHost) HasCommand(ctx context.Context, name string) (bool, error) {
	if name == "systemctl" && f.noSystemd {
		return false, nil
	}
	return true, nil
}

func (f *fakeHost) UploadDir(ctx context.Context, src, dst string) error { return nil }
func (f *fakeHost) MkdirAll(ctx context.Context, dir string) error       { return nil }
func (f *fakeHost) RemoveAll(ctx context.Context, p string) error        { return nil }
func (f *fakeHost) Label() string                                        { return "fake" }

func (f *fakeHost) ran(substr string) bool {
	for _, c := range f.cmds {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func TestDetectArch(t *testing.T) {
	tests := []struct {
		uname   string
		want    Arch
		wantErr string
	}{
		{"Linux x86_64", ArchAMD64, ""},
		{"Linux amd64", ArchAMD64, ""},
		{"Linux aarch64", ArchARM64, ""},
		{"Linux arm64", ArchARM64, ""},
		{"Darwin arm64", "", "targets Linux"},
		{"Linux riscv64", "", "unsupported architecture"},
	}
	for _, tc := range tests {
		t.Run(tc.uname, func(t *testing.T) {
			got, err := DetectArch(context.Background(), newFake(tc.uname))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error mentioning %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q should mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("arch = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstallUploadsAndEnables(t *testing.T) {
	f := newFake("Linux x86_64")

	bin := filepath.Join(t.TempDir(), "pilotd")
	if err := os.WriteFile(bin, []byte("ELF-ish"), 0o755); err != nil {
		t.Fatal(err)
	}

	layout := release.NewLayout("")
	res, err := Install(context.Background(), f, bin, Options{
		Host: "web-1", Layout: layout,
		Caddy: caddy.Paths{Caddyfile: "/etc/caddy/Caddyfile", SnippetDir: "/etc/caddy/pilot.d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Installed || !res.Restarted || res.Arch != ArchAMD64 {
		t.Errorf("result = %+v", res)
	}

	// The binary is staged beside its target and renamed in, so a running
	// daemon never reads a half-written file.
	if _, ok := f.files["/opt/pilot/bin/pilotd.new"]; !ok {
		t.Error("binary should be staged before being moved into place")
	}
	if !f.ran("mv -f /opt/pilot/bin/pilotd.new /opt/pilot/bin/pilotd") {
		t.Errorf("expected an atomic rename, ran: %v", f.cmds)
	}

	unit, ok := f.files[UnitPath]
	if !ok {
		t.Fatal("systemd unit not written")
	}
	for _, want := range []string{
		"ExecStart=/opt/pilot/bin/pilotd serve",
		"--socket /run/pilot.sock",
		"--root /opt/pilot",
		"--host web-1",
		"--caddyfile /etc/caddy/Caddyfile",
		"--snippet-dir /etc/caddy/pilot.d",
		"Restart=always",
	} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}

	for _, want := range []string{"daemon-reload", "enable pilotd.service", "restart pilotd.service"} {
		if !f.ran(want) {
			t.Errorf("expected to run %q, ran: %v", want, f.cmds)
		}
	}
}

func TestInstallRequiresSystemd(t *testing.T) {
	f := newFake("Linux x86_64")
	f.noSystemd = true

	bin := filepath.Join(t.TempDir(), "pilotd")
	os.WriteFile(bin, []byte("x"), 0o755)

	_, err := Install(context.Background(), f, bin, Options{Host: "web-1", Layout: release.NewLayout("")})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "systemd") {
		t.Errorf("error should explain: %v", err)
	}
	if len(f.files) != 0 {
		t.Error("nothing should have been written to a host that cannot run the agent")
	}
}

// A unit that starts and then immediately exits must not read as success.
func TestInstallReportsStartFailureWithSomewhereToLook(t *testing.T) {
	f := newFake("Linux x86_64")
	f.failStart = true

	bin := filepath.Join(t.TempDir(), "pilotd")
	os.WriteFile(bin, []byte("x"), 0o755)

	_, err := Install(context.Background(), f, bin, Options{Host: "web-1", Layout: release.NewLayout("")})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "journalctl") {
		t.Errorf("error should point at the journal: %v", err)
	}
}

func TestRenderUnitOmitsUnsetOptions(t *testing.T) {
	unit := RenderUnit(UnitOptions{
		Binary: "/opt/pilot/bin/pilotd",
		Socket: "/run/pilot.sock",
		Root:   "/opt/pilot",
	})
	for _, unwanted := range []string{"--host", "--caddyfile", "--snippet-dir", "--caddy-admin"} {
		if strings.Contains(unit, unwanted) {
			t.Errorf("unset option %q should be omitted:\n%s", unwanted, unit)
		}
	}
	if !strings.Contains(unit, "managed by pilot") {
		t.Error("unit should say who owns it")
	}
}

func TestRenderUnitQuotesAwkwardPaths(t *testing.T) {
	unit := RenderUnit(UnitOptions{
		Binary: "/opt/pilot/bin/pilotd",
		Socket: "/run/pilot.sock",
		Root:   "/opt/my pilot",
		Host:   "web-1",
	})
	if !strings.Contains(unit, "'/opt/my pilot'") {
		t.Errorf("a path with a space must be quoted:\n%s", unit)
	}
}

func TestSourceResolvePrefersExplicitPath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "custom-pilotd")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	path, origin, cleanup, err := Source{Explicit: bin}.Resolve(context.Background(), ArchAMD64)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if path != bin || !strings.Contains(origin, "given") {
		t.Errorf("path = %q, origin = %q", path, origin)
	}
}

func TestSourceResolveReportsMissingBinaryUsefully(t *testing.T) {
	_, _, cleanup, err := Source{Explicit: "/nope/pilotd"}.Resolve(context.Background(), ArchAMD64)
	defer cleanup()
	if err == nil {
		t.Fatal("want an error")
	}

	// With no explicit path and no module to build from, the error should tell
	// the operator exactly how to produce one.
	_, _, cleanup2, err := Source{}.Resolve(context.Background(), ArchARM64)
	defer cleanup2()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "GOARCH=arm64") {
		t.Errorf("error should give the build command: %v", err)
	}
}

func TestUninstallLeavesReleasesAlone(t *testing.T) {
	f := newFake("Linux x86_64")
	if err := Uninstall(context.Background(), f, release.NewLayout("")); err != nil {
		t.Fatal(err)
	}
	if !f.ran("disable --now pilotd.service") || !f.ran("rm -f /opt/pilot/bin/pilotd") {
		t.Errorf("ran: %v", f.cmds)
	}
	for _, c := range f.cmds {
		if strings.Contains(c, "/opt/pilot/services") {
			t.Errorf("uninstall must not touch deployed services: %q", c)
		}
	}
}
