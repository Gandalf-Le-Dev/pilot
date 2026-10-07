// Package install puts the agent onto a host.
//
// Getting pilotd onto a machine is the step that makes everything else in
// phase 2 real: without it, auto-rollback and drift detection are code nobody
// can run.
package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/server"
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
// Every path here ties the agent to the CLI that installs it: the release
// tarball's sibling binary, the agent published with this CLI's own release,
// or a build from this checkout. That is deliberate, and it is why there is no
// longer an option to point at an arbitrary binary.
//
// There used to be one. It was reached for exactly once, to work around an
// agent that rejected a config field the CLI had learned — and by making the
// mismatch survivable it removed the pressure to fix it. An agent installed
// from somewhere other than the CLI's own release is an agent whose protocol
// and schema nobody has checked, installed as root. The supported way to run
// an agent you built is to run the pilot you built alongside it.
type Source struct {
	// Version is the CLI's own version. A released build fetches the agent
	// published alongside it, which is what makes protocol skew impossible
	// rather than merely unlikely.
	Version string

	// BaseURL overrides the release download root, for tests.
	BaseURL string

	// ModuleDir is the Pilot checkout to build from, when one is available.
	ModuleDir string
}

// executable is os.Executable, indirected so a test can place a fake pilot in
// a temporary directory and exercise the sibling lookup.
var executable = os.Executable

// Resolve returns a local path to a pilotd binary for arch, plus a description
// of where it came from for the operator to see.
func (s Source) Resolve(ctx context.Context, arch Arch) (path, origin string, cleanup func(), err error) {
	noop := func() {}

	// A development build compiles its own agent whenever it can, before
	// looking at anything on disk.
	//
	// The sibling lookup below is a filename match, nothing more: it installs
	// whatever `pilotd-linux-<arch>` happens to sit next to the pilot binary,
	// as root. In a release tarball those two shipped together and that is
	// exactly right. For a `go build` dropped in a shared directory it is a
	// coincidence — and it bit immediately, picking up an agent built before a
	// protocol bump and installing it over the fixed one. Source is the only
	// thing that provably matches a build that has no version to match against.
	if s.ModuleDir != "" && !IsReleaseVersion(s.Version) {
		built, err := buildAgent(ctx, s.ModuleDir, arch)
		if err != nil {
			return "", "", noop, err
		}
		return built, fmt.Sprintf("built from %s", s.ModuleDir), func() { os.Remove(built) }, nil
	}

	// A sibling of the running pilot binary, as a release tarball lays out.
	if self, err := executable(); err == nil {
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
				"the agent must come from the same release as this pilot, so there is\n"+
				"nothing safe to fall back to — retry, or run pilot from a checkout to\n"+
				"build a matching agent", arch, err)
		}
	}

	if s.ModuleDir == "" {
		return "", "", noop, fmt.Errorf(
			"no pilotd binary for linux/%s\n"+
				"this build (%s) has no matching release to download from, and is not\n"+
				"running inside a Pilot checkout to build one\n\n"+
				"build the agent next to this binary:\n"+
				"    GOOS=linux GOARCH=%s go build -o %s ./cmd/pilotd\n"+
				"and put it beside pilot, or run pilot from the checkout instead",
			arch, orDefault(s.Version, "dev"), arch, AssetName(arch))
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
			"install Go, or use a released build of pilot, which downloads its own agent")
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

const (
	// ServerUnitPath is where the status server's unit lives.
	ServerUnitPath = "/etc/systemd/system/pilot-server.service"

	// ServerConfigPath is the server's configuration. Readable by root
	// alone: it holds notifier webhooks, which are credentials. The unit
	// hands it to the server as a systemd credential, so the unprivileged
	// process reads a copy without the file ever being opened to it.
	ServerConfigPath = "/etc/pilot/" + server.CredentialName
)

// minSystemd is the first systemd with LoadCredential=, which is how the
// server receives its configuration. On an older one the unit would load,
// ignore the line, and start a server with no configuration.
const minSystemd = 247

// Server start is confirmed by watching the unit for a while, because
// systemctl returns once the binary has been executed, and a server that
// rejects its configuration exits a moment later.
var (
	serverSettle = 3 * time.Second
	serverPoll   = 500 * time.Millisecond
)

// InstallServer writes the status server's configuration and unit, restarts
// it, and confirms it stayed up.
//
// Always a restart, never a reload: the configuration arrives as a
// credential, which systemd reads only when the unit starts, and the binary
// it runs was likely just replaced.
func InstallServer(ctx context.Context, ex transport.Executor, binary string, config []byte) error {
	if err := requireSystemdVersion(ctx, ex, minSystemd); err != nil {
		return err
	}

	// The server runs as a dynamic user, not root, so it must be able to
	// reach the binary. Pilot owns these two directories; a umask that left
	// them closed would otherwise surface as a crash loop with "permission
	// denied" in the journal.
	bin := filepath.Dir(binary)
	if r, err := ex.Run(ctx, transport.Join("chmod", "0755", filepath.Dir(bin), bin)); err != nil {
		return err
	} else if !r.OK() {
		return fmt.Errorf("opening %s to the status server: %w", bin, r.Err())
	}

	if err := ex.MkdirAll(ctx, filepath.Dir(ServerConfigPath)); err != nil {
		return err
	}
	if err := ex.WriteFile(ctx, ServerConfigPath, config, "0600"); err != nil {
		return fmt.Errorf("writing the status server configuration: %w", err)
	}
	if err := ex.WriteFile(ctx, ServerUnitPath, []byte(ServerUnit(binary)), "0644"); err != nil {
		return fmt.Errorf("writing the status server unit: %w", err)
	}

	script := strings.Join([]string{
		"systemctl daemon-reload",
		"systemctl enable pilot-server.service",
		"systemctl restart pilot-server.service",
	}, "\n")
	if r, err := ex.RunScript(ctx, script); err != nil {
		return err
	} else if !r.OK() {
		return serverStartError(ctx, ex, r.Err().Error())
	}
	return waitServerActive(ctx, ex)
}

// waitServerActive watches the unit through the settle window and fails on
// the first sign it did not stay up.
func waitServerActive(ctx context.Context, ex transport.Executor) error {
	deadline := time.Now().Add(serverSettle)
	for {
		r, err := ex.Run(ctx, "systemctl is-active pilot-server.service")
		if err != nil {
			return err
		}
		state := strings.TrimSpace(r.Stdout)
		if state != "active" && state != "activating" {
			return serverStartError(ctx, ex, "the unit is "+orDefault(state, "not running"))
		}
		if !time.Now().Before(deadline) {
			if state != "active" {
				return serverStartError(ctx, ex, "the unit is still "+state)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(serverPoll):
		}
	}
}

// serverStartError carries the unit's last journal lines, since the reason a
// server refused to start (a bad configuration, root) is printed there.
func serverStartError(ctx context.Context, ex transport.Executor, why string) error {
	msg := "the status server did not start: " + why
	if r, err := ex.Run(ctx, "journalctl -u pilot-server.service -n 10 --no-pager -o cat"); err == nil && strings.TrimSpace(r.Stdout) != "" {
		msg += "\n" + strings.TrimSpace(r.Stdout)
	} else {
		msg += "\nsee what happened with:  journalctl -u pilot-server -n 50 --no-pager"
	}
	return errors.New(msg)
}

func requireSystemdVersion(ctx context.Context, ex transport.Executor, least int) error {
	r, err := ex.Run(ctx, "systemctl --version")
	if err != nil {
		return err
	}
	// "systemd 252 (252.22-1~deb12u1)"
	fields := strings.Fields(r.Stdout)
	if len(fields) < 2 {
		return fmt.Errorf("cannot read the systemd version from %q", firstLine(r.Stdout))
	}
	v, err := strconv.Atoi(fields[1])
	if err != nil {
		return fmt.Errorf("cannot read the systemd version from %q", firstLine(r.Stdout))
	}
	if v < least {
		return fmt.Errorf("the status server needs systemd %d or later for LoadCredential=; this host runs %d", least, v)
	}
	return nil
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return head
}

// scriptRunner is all RemoveServer needs, so doctor can call it with the
// connection it already holds.
type scriptRunner interface {
	RunScript(ctx context.Context, body string) (transport.Result, error)
}

// RemoveServer stops and deletes a status server, reporting whether there was
// one. Idempotent, and quiet on a host that never ran one.
//
// A server left behind when status.host moves, or the status block goes, is
// worse than useless: it keeps its old configuration and pages about every
// host that no longer reports to it, forever.
func RemoveServer(ctx context.Context, r scriptRunner) (bool, error) {
	unit, cfg := transport.Quote(ServerUnitPath), transport.Quote(ServerConfigPath)
	script := strings.Join([]string{
		"if [ ! -e " + unit + " ] && [ ! -e " + cfg + " ]; then exit 0; fi",
		"systemctl disable --now pilot-server.service 2>/dev/null || true",
		"rm -f " + unit + " " + cfg,
		"systemctl daemon-reload",
		"echo removed",
	}, "\n")
	res, err := r.RunScript(ctx, script)
	if err != nil {
		return false, err
	}
	if err := res.Err(); err != nil {
		return false, fmt.Errorf("removing the status server: %w", err)
	}
	return strings.TrimSpace(res.Stdout) == "removed", nil
}

// ServerUnit renders the status server's systemd unit.
//
// The agent runs as root because it must; this process must not. It parses
// reports from the tailnet and serves the internet through Caddy, so it gets
// a fresh unprivileged user every start, no capabilities, a read-only view of
// the filesystem and nothing beyond IP sockets. Its configuration comes in as
// a credential, the one file it ever reads.
func ServerUnit(binary string) string {
	return fmt.Sprintf(`# managed by pilot — do not edit
# regenerate with: pilot agent upgrade <status host>
[Unit]
Description=Pilot status server
Documentation=https://github.com/Gandalf-Le-Dev/pilot
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=exec
ExecStart=%s
Restart=always
RestartSec=5s
DynamicUser=yes
LoadCredential=%s:%s
NoNewPrivileges=yes
CapabilityBoundingSet=
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
UMask=0077
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, transport.Join(binary, "server"), server.CredentialName, ServerConfigPath)
}

// Uninstall stops and removes the agent, leaving releases untouched. A status
// server goes with it: its binary is the agent's, so left behind its unit
// would crash-loop at the next boot.
func Uninstall(ctx context.Context, ex transport.Executor, layout release.Layout) error {
	if _, err := RemoveServer(ctx, ex); err != nil {
		return err
	}
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
