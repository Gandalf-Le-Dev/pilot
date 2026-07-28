// Package install puts the agent onto a host.
//
// Getting pilotd onto a machine is the step that makes everything else in
// phase 2 real: without it, auto-rollback and drift detection are code nobody
// can run.
package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// UnitPath is where the agent's systemd unit lives.
const UnitPath = "/etc/systemd/system/pilotd.service"

// Arch is a target architecture Pilot can install for.
type Arch string

const (
	ArchAMD64 Arch = "amd64"
	ArchARM64 Arch = "arm64"
)

// DetectArch asks the host what it is.
//
// Only Linux is supported as a target: the agent talks to systemd and to the
// Docker socket, and a friendly error beats a binary that will not start.
func DetectArch(ctx context.Context, ex transport.Executor) (Arch, error) {
	res, err := ex.Run(ctx, "uname -sm")
	if err != nil {
		return "", err
	}
	if !res.OK() {
		return "", fmt.Errorf("could not determine the host architecture: %w", res.Err())
	}

	fields := strings.Fields(res.Out())
	if len(fields) != 2 {
		return "", fmt.Errorf("unexpected `uname -sm` output: %q", res.Out())
	}
	if !strings.EqualFold(fields[0], "linux") {
		return "", fmt.Errorf("the agent targets Linux hosts; this host reports %q", fields[0])
	}

	switch fields[1] {
	case "x86_64", "amd64":
		return ArchAMD64, nil
	case "aarch64", "arm64":
		return ArchARM64, nil
	}
	return "", fmt.Errorf("unsupported architecture %q (Pilot builds the agent for amd64 and arm64)", fields[1])
}

// Source locates a pilotd binary for a target architecture.
//
// The search order goes from most explicit to most convenient, and every step
// is something an operator can reason about. Building from source is last but
// is the common case for someone running Pilot out of its own repository, and
// it avoids requiring any release infrastructure to exist at all.
type Source struct {
	// Explicit is a path given on the command line or in the environment.
	Explicit string

	// Version is the CLI's own version. A released build fetches the agent
	// published alongside it, which is what makes protocol skew impossible
	// rather than merely unlikely.
	Version string

	// BaseURL overrides the release download root, for tests.
	BaseURL string

	// ModuleDir is the Pilot checkout to build from, when one is available.
	ModuleDir string
}

// Resolve returns a local path to a pilotd binary for arch, plus a description
// of where it came from for the operator to see.
func (s Source) Resolve(ctx context.Context, arch Arch) (path, origin string, cleanup func(), err error) {
	noop := func() {}

	if s.Explicit != "" {
		if _, err := os.Stat(s.Explicit); err != nil {
			return "", "", noop, fmt.Errorf("agent binary %s: %w", s.Explicit, err)
		}
		return s.Explicit, "given path", noop, nil
	}

	// A sibling of the running pilot binary, as a release tarball would lay out.
	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), AssetName(arch))
		if _, err := os.Stat(candidate); err == nil {
			return candidate, "alongside the pilot binary", noop, nil
		}
	}

	// The agent published with this CLI's own release. Cached after the first
	// fetch, and always checksum-verified.
	if IsReleaseVersion(s.Version) {
		path, origin, err := s.download(ctx, arch)
		if err == nil {
			return path, origin, noop, nil
		}
		// A download failure is only recoverable if we can build instead.
		if s.ModuleDir == "" {
			return "", "", noop, fmt.Errorf("could not obtain the agent for linux/%s: %w\n"+
				"pass --agent-binary <path> to install one you already have", arch, err)
		}
	}

	if s.ModuleDir == "" {
		return "", "", noop, fmt.Errorf(
			"no pilotd binary for linux/%s\n"+
				"this build (%s) has no matching release to download from\n"+
				"build one with:  GOOS=linux GOARCH=%s go build -o %s ./cmd/pilotd\n"+
				"then pass --agent-binary %s",
			arch, orDefault(s.Version, "dev"), arch, AssetName(arch), AssetName(arch))
	}

	built, err := buildAgent(ctx, s.ModuleDir, arch)
	if err != nil {
		return "", "", noop, err
	}
	return built, fmt.Sprintf("built from %s", s.ModuleDir), func() { os.Remove(built) }, nil
}

// buildAgent cross-compiles pilotd for the target.
func buildAgent(ctx context.Context, moduleDir string, arch Arch) (string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("no prebuilt agent found and Go is not installed to build one:\n" +
			"install Go, or pass --agent-binary <path>")
	}

	out := filepath.Join(os.TempDir(), fmt.Sprintf("pilotd-linux-%s-%d", arch, os.Getpid()))
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/pilotd")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+string(arch), "CGO_ENABLED=0")

	if combined, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building the agent for linux/%s failed: %w\n%s", arch, err, combined)
	}
	return out, nil
}

// Options configures an installation.
type Options struct {
	Host   string
	Layout release.Layout
	Caddy  caddy.Paths
	Socket string
}

// Result describes what an install did.
type Result struct {
	Arch      Arch
	Origin    string
	Installed bool
	Restarted bool
	Info      *proto.Info
}

// Install uploads the agent, writes its systemd unit, and starts it.
//
// It is idempotent: running it against an already-current host replaces the
// binary and restarts the daemon, which is exactly what an upgrade needs.
func Install(ctx context.Context, ex transport.Executor, binary string, opts Options) (*Result, error) {
	socket := opts.Socket
	if socket == "" {
		socket = proto.DefaultSocket
	}

	res := &Result{}

	arch, err := DetectArch(ctx, ex)
	if err != nil {
		return nil, err
	}
	res.Arch = arch

	if err := requireSystemd(ctx, ex); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}

	// Upload beside the target and rename into place, so a running daemon is
	// never reading a half-written binary.
	target := opts.Layout.Agent()
	staged := target + ".new"
	if err := ex.MkdirAll(ctx, filepath.Dir(target)); err != nil {
		return nil, err
	}
	if err := ex.WriteFile(ctx, staged, data, "0755"); err != nil {
		return nil, fmt.Errorf("uploading the agent: %w", err)
	}
	if r, err := ex.Run(ctx, transport.Join("mv", "-f", staged, target)); err != nil {
		return nil, err
	} else if !r.OK() {
		return nil, fmt.Errorf("installing the agent: %w", r.Err())
	}
	res.Installed = true

	unit := RenderUnit(UnitOptions{
		Binary: target, Socket: socket, Root: opts.Layout.Root,
		Host: opts.Host, Caddy: opts.Caddy,
	})
	if err := ex.WriteFile(ctx, UnitPath, []byte(unit), "0644"); err != nil {
		return nil, fmt.Errorf("writing the systemd unit: %w", err)
	}

	script := strings.Join([]string{
		"systemctl daemon-reload",
		"systemctl enable pilotd.service",
		"systemctl restart pilotd.service",
	}, "\n")
	if r, err := ex.RunScript(ctx, script); err != nil {
		return nil, err
	} else if !r.OK() {
		return nil, fmt.Errorf("starting the agent: %w\n%s", r.Err(),
			journalHint())
	}
	res.Restarted = true

	return res, nil
}

func requireSystemd(ctx context.Context, ex transport.Executor) error {
	ok, err := ex.HasCommand(ctx, "systemctl")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("systemctl not found: the agent is supervised by systemd, " +
			"so a host without it cannot run one")
	}
	return nil
}

func journalHint() string {
	return "see what happened with:  journalctl -u pilotd -n 50 --no-pager"
}

// UnitOptions parameterises the systemd unit.
type UnitOptions struct {
	Binary string
	Socket string
	Root   string
	Host   string
	Caddy  caddy.Paths
}

// RenderUnit produces the systemd unit for the agent.
//
// Restart=always with a delay is the point: if the agent dies, systemd brings
// it back, and monitoring resumes without anyone noticing. Deploys keep working
// meanwhile because the CLI can drive a host directly.
func RenderUnit(o UnitOptions) string {
	args := []string{
		o.Binary, "serve",
		"--socket", o.Socket,
		"--root", o.Root,
	}
	if o.Host != "" {
		args = append(args, "--host", o.Host)
	}
	if o.Caddy.Caddyfile != "" {
		args = append(args, "--caddyfile", o.Caddy.Caddyfile)
	}
	if o.Caddy.SnippetDir != "" {
		args = append(args, "--snippet-dir", o.Caddy.SnippetDir)
	}
	if o.Caddy.Admin != "" {
		args = append(args, "--caddy-admin", o.Caddy.Admin)
	}

	return fmt.Sprintf(`# managed by pilot — do not edit
# regenerate with: pilot bootstrap %s
[Unit]
Description=Pilot agent
Documentation=https://github.com/Gandalf-Le-Dev/pilot
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=always
RestartSec=5s
# The agent activates releases and writes Caddy config, so it needs root.
User=root
# Keep the socket's directory across a reboot.
RuntimeDirectory=pilot
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, o.Host, transport.Join(args...))
}

// Uninstall stops and removes the agent, leaving releases untouched.
func Uninstall(ctx context.Context, ex transport.Executor, layout release.Layout) error {
	script := strings.Join([]string{
		"systemctl disable --now pilotd.service 2>/dev/null || true",
		"rm -f " + transport.Quote(UnitPath),
		"systemctl daemon-reload",
		"rm -f " + transport.Quote(layout.Agent()),
	}, "\n")

	res, err := ex.RunScript(ctx, script)
	if err != nil {
		return err
	}
	return res.Err()
}
