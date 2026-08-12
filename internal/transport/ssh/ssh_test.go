package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// stub installs a fake `ssh` executable that records how it was invoked and
// replays a scripted response. It lets the real exec paths — argv, stdin
// piping, exit codes — be tested without a server.
type stub struct {
	bin      string
	argsLog  string
	stdinLog string
}

func newStub(t *testing.T, exitCode int, stdout, stderr string) *stub {
	t.Helper()
	dir := t.TempDir()
	s := &stub{
		bin:      filepath.Join(dir, "fake-ssh"),
		argsLog:  filepath.Join(dir, "args"),
		stdinLog: filepath.Join(dir, "stdin"),
	}

	// Both logs append, because one client call can invoke ssh more than once
	// — an upload streams the archive, then normalizes permissions.
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + s.argsLog + "; done\n" +
		"cat >> " + s.stdinLog + "\n"
	if stdout != "" {
		script += "printf '%s' " + transport.Quote(stdout) + "\n"
	}
	if stderr != "" {
		script += "printf '%s' " + transport.Quote(stderr) + " >&2\n"
	}
	script += "exit " + itoa(exitCode) + "\n"

	if err := os.WriteFile(s.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (s *stub) args(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(s.argsLog)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func (s *stub) stdin(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.stdinLog)
	if err != nil {
		return ""
	}
	return string(b)
}

func (s *stub) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{
		Name: "web-1", Address: "web1.example.com", User: "deploy",
		Binary: s.bin, ControlDir: t.TempDir(), BatchMode: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func contains(args []string, want ...string) bool {
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	return strings.Contains(joined, "\x00"+strings.Join(want, "\x00")+"\x00")
}

func TestConfigOptions(t *testing.T) {
	cfg := Config{
		Name: "box-1", Address: "10.0.0.5", User: "deploy", Port: 2222,
		IdentityFile: "/keys/id_ed25519", ProxyJump: "deploy@web1.example.com",
		ControlDir: "/tmp/cm", BatchMode: true,
	}
	args := cfg.Args("uptime")

	for _, want := range [][]string{
		{"-o", "ControlMaster=auto"},
		{"-o", "BatchMode=yes"},
		{"-p", "2222"},
		{"-i", "/keys/id_ed25519"},
		{"-J", "deploy@web1.example.com"},
	} {
		if !contains(args, want...) {
			t.Errorf("args missing %v:\n%v", want, args)
		}
	}
	if args[len(args)-1] != "uptime" || args[len(args)-2] != "deploy@10.0.0.5" {
		t.Errorf("target and command should be last, got: %v", args[len(args)-2:])
	}
}

func TestConfigOmitsUnsetOptions(t *testing.T) {
	cfg := Config{Address: "example.com", ControlDir: "/tmp/cm"}
	args := cfg.Args("")
	for _, unwanted := range []string{"-p", "-i", "-J", "BatchMode=yes"} {
		for _, a := range args {
			if a == unwanted {
				t.Errorf("unset option %q should be omitted:\n%v", unwanted, args)
			}
		}
	}
	if args[len(args)-1] != "example.com" {
		t.Errorf("bare target expected when no user is set, got %q", args[len(args)-1])
	}
}

// Unix socket paths are capped near 104 bytes; a long user/host would blow the
// limit and produce a baffling error from ssh itself.
func TestControlPathStaysShort(t *testing.T) {
	cfg := Config{
		Address:    strings.Repeat("very-long-hostname.", 12) + "example.com",
		User:       strings.Repeat("u", 60),
		ControlDir: "/Users/somebody/.pilot/cm",
		Port:       22,
	}
	got := cfg.ControlPath()
	if len(got) > 100 {
		t.Errorf("ControlPath is %d bytes, too long for a unix socket: %s", len(got), got)
	}

	// Distinct hosts must not share a socket.
	other := cfg
	other.Address = "different.example.com"
	if other.ControlPath() == got {
		t.Error("different hosts collided onto one control socket")
	}
}

// The trailing slash on the source is load-bearing: without it rsync would
// nest the directory inside the destination instead of syncing its contents.
func TestRsyncArgs(t *testing.T) {
	cfg := Config{Address: "web1.example.com", User: "deploy", ControlDir: "/tmp/cm"}
	args := cfg.RsyncArgs("/local/dist", "/opt/pilot/services/blog/releases/0042-abc1234")

	src := args[len(args)-2]
	dst := args[len(args)-1]
	if src != "/local/dist/" {
		t.Errorf("source = %q, want a trailing slash", src)
	}
	if dst != "deploy@web1.example.com:/opt/pilot/services/blog/releases/0042-abc1234/" {
		t.Errorf("destination = %q", dst)
	}
	if !contains(args, "--delete") {
		t.Errorf("staging should mirror exactly: %v", args)
	}
	if !strings.Contains(cfg.RsyncShell(), "ControlPath=") {
		t.Errorf("rsync should reuse the multiplexed connection: %s", cfg.RsyncShell())
	}
}

func TestRunCapturesOutput(t *testing.T) {
	s := newStub(t, 0, "hello\n", "")
	res, err := s.client(t).Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || res.Out() != "hello" {
		t.Errorf("res = %+v", res)
	}
	if !contains(s.args(t), "deploy@web1.example.com", "echo hello") {
		t.Errorf("remote command not passed through: %v", s.args(t))
	}
}

// A non-zero exit is an answer, not a transport failure — `test -f x` returning
// 1 must not read as "the host is broken".
func TestNonZeroExitIsNotAnError(t *testing.T) {
	s := newStub(t, 1, "", "")
	res, err := s.client(t).Run(context.Background(), "test -f /nope")
	if err != nil {
		t.Fatalf("a failed remote command should not be a transport error: %v", err)
	}
	if res.OK() || res.ExitCode != 1 {
		t.Errorf("res = %+v", res)
	}
	if res.Err() == nil {
		t.Error("Result.Err should report the failure for callers that need success")
	}
}

func TestConnectionFailureIsDistinguished(t *testing.T) {
	s := newStub(t, 255, "", "ssh: connect to host web1 port 22: Connection refused\n")
	_, err := s.client(t).Run(context.Background(), "uptime")
	if err == nil {
		t.Fatal("want a connection error")
	}
	if !IsUnreachable(err) {
		t.Errorf("error should classify as unreachable, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "web-1") {
		t.Errorf("error should name the host: %v", err)
	}
}

// Exit 255 from a remote command that merely returned 255 must not be
// misreported as the host being down.
func TestExit255WithoutConnectionMarkerIsNotUnreachable(t *testing.T) {
	s := newStub(t, 255, "", "application said no\n")
	_, err := s.client(t).Run(context.Background(), "/opt/thing")
	if err != nil && IsUnreachable(err) {
		t.Errorf("should not be classified as a connection failure: %v", err)
	}
}

func TestRunScriptPipesBodyOnStdin(t *testing.T) {
	s := newStub(t, 0, "", "")
	if _, err := s.client(t).RunScript(context.Background(), "echo one\necho two"); err != nil {
		t.Fatal(err)
	}
	if !contains(s.args(t), "sh -s") {
		t.Errorf("script should be fed to a remote shell: %v", s.args(t))
	}
	got := s.stdin(t)
	if !strings.HasPrefix(got, "set -eu\n") || !strings.Contains(got, "echo two") {
		t.Errorf("stdin = %q", got)
	}
}

// The payload rides on stdin after the script, so there is no command-line
// length limit and no quoting hazard — which matters for .env files.
func TestWriteFileSendsPayloadOnStdin(t *testing.T) {
	s := newStub(t, 0, "", "")
	secret := "DATABASE_URL=postgres://user:pw@db/app\nLOG_LEVEL=debug\n"

	if err := s.client(t).WriteFile(context.Background(), "/opt/pilot/services/api/releases/0042-abc1234/.env", []byte(secret), "0600"); err != nil {
		t.Fatal(err)
	}

	// The payload owns stdin entirely — no script interleaved with it. Mixing
	// the two made the shell execute part of the payload as commands.
	if got := s.stdin(t); got != secret {
		t.Errorf("stdin should be exactly the payload, got:\n%q", got)
	}
	joined := strings.Join(s.args(t), " ")
	if !strings.Contains(joined, "chmod 0600") || !strings.Contains(joined, "mv -f") {
		t.Errorf("the command belongs in argv:\n%v", s.args(t))
	}
	// The secret must not appear in the argument vector, which is visible in
	// the host's process list.
	for _, a := range s.args(t) {
		if strings.Contains(a, "postgres://") {
			t.Errorf("secret leaked into argv: %q", a)
		}
	}
}

func TestReadFileAndExists(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		s := newStub(t, 0, "contents\n", "")
		b, err := s.client(t).ReadFile(context.Background(), "/etc/caddy/Caddyfile")
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "contents\n" {
			t.Errorf("got %q", b)
		}
	})

	t.Run("read failure is reported", func(t *testing.T) {
		s := newStub(t, 1, "", "cat: no such file\n")
		if _, err := s.client(t).ReadFile(context.Background(), "/nope"); err == nil {
			t.Error("want an error")
		}
	})

	t.Run("exists", func(t *testing.T) {
		s := newStub(t, 0, "", "")
		ok, err := s.client(t).Exists(context.Background(), "/opt/pilot")
		if err != nil || !ok {
			t.Errorf("got %v, %v", ok, err)
		}
	})

	t.Run("absent", func(t *testing.T) {
		s := newStub(t, 1, "", "")
		ok, err := s.client(t).Exists(context.Background(), "/nope")
		if err != nil || ok {
			t.Errorf("got %v, %v", ok, err)
		}
	})
}

// This runs as root on a real server. A path built from an empty variable must
// never expand into something catastrophic.
func TestRemoveAllRefusesDangerousPaths(t *testing.T) {
	s := newStub(t, 0, "", "")
	c := s.client(t)

	for _, bad := range []string{"", "/", ".", "relative/path", "/opt", "/etc"} {
		if err := c.RemoveAll(context.Background(), bad); err == nil {
			t.Errorf("RemoveAll(%q) should have been refused", bad)
		}
	}
	if got := s.args(t); got != nil {
		t.Errorf("refusals must not reach the host, but ssh ran with: %v", got)
	}

	if err := c.RemoveAll(context.Background(), "/opt/pilot/services/api/releases/0001-aaaaaaa"); err != nil {
		t.Errorf("a legitimate release path should be removable: %v", err)
	}
}

func TestWriteTar(t *testing.T) {
	src := t.TempDir()
	mkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(src, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		if err := os.WriteFile(filepath.Join(src, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("assets")
	write("index.html", "<h1>hi</h1>")
	write("assets/app.js", "console.log(1)")
	if err := os.Symlink("index.html", filepath.Join(src, "home.html")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeTar(src, &buf); err != nil {
		t.Fatal(err)
	}

	found := map[string]string{}
	links := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(hdr.Name, "..") || strings.HasPrefix(hdr.Name, "/") {
			t.Errorf("unsafe archive path: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			links[hdr.Name] = hdr.Linkname
		case tar.TypeReg:
			b, _ := io.ReadAll(tr)
			found[hdr.Name] = string(b)
		}
	}

	if found["index.html"] != "<h1>hi</h1>" || found["assets/app.js"] != "console.log(1)" {
		t.Errorf("archive contents = %v", found)
	}
	// Symlinks are preserved rather than dereferenced, so a build output
	// containing one doesn't get silently duplicated.
	if links["home.html"] != "index.html" {
		t.Errorf("symlink not preserved: %v", links)
	}
}

func TestUploadDirStreamsTar(t *testing.T) {
	s := newStub(t, 0, "", "")
	c := s.client(t)
	// Force the tar path by pointing rsync at something that doesn't exist.
	cfg := c.Config()
	cfg.RsyncBinary = filepath.Join(t.TempDir(), "no-such-rsync")
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.UploadDir(context.Background(), src, "/opt/pilot/services/blog/releases/0042-abc1234"); err != nil {
		t.Fatal(err)
	}

	args := s.args(t)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "tar -C") || !strings.Contains(joined, "mkdir -p") {
		t.Errorf("expected a remote extract command: %v", args)
	}
	if !strings.Contains(s.stdin(t), "index.html") {
		t.Error("archive should have been streamed on stdin")
	}
}

// The transfer preserves the staging directory's owner and 0700 mode, which
// are meaningless on the host; without the normalize pass a static release
// arrives unreadable by the web server.
func TestUploadDirNormalizesPermissions(t *testing.T) {
	s := newStub(t, 0, "", "")
	cfg := s.client(t).Config()
	cfg.RsyncBinary = filepath.Join(t.TempDir(), "no-such-rsync")
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.UploadDir(context.Background(), src, "/opt/pilot/services/blog/releases/0042-abc1234"); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(s.args(t), " ")
	if !strings.Contains(joined, "chmod -R u=rwX,go=rX") {
		t.Errorf("uploaded tree should be made world-readable: %v", joined)
	}
	// A plain deploy user cannot chown; the files already belong to it.
	if strings.Contains(joined, "chown") {
		t.Errorf("chown should be reserved for hosts where commands run as root: %v", joined)
	}
}

func TestUploadDirNormalizesOwnershipWithSudo(t *testing.T) {
	s := newStub(t, 0, "", "")
	cfg := s.client(t).Config()
	cfg.Sudo = true
	cfg.RsyncBinary = filepath.Join(t.TempDir(), "no-such-rsync")
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.UploadDir(context.Background(), src, "/opt/pilot/services/blog/releases/0042-abc1234"); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(s.args(t), " ")
	if !strings.Contains(joined, "chown -R root:root") {
		t.Errorf("root transport should re-own the release: %v", joined)
	}
}

// The rsync path is where the bug lived: --archive received by root recreates
// the operator's uid and mode on the host, so the normalize pass must follow
// rsync too, not just tar.
func TestUploadDirNormalizesAfterRsync(t *testing.T) {
	s := newStub(t, 0, "", "")
	cfg := s.client(t).Config()

	fakeRsync := filepath.Join(t.TempDir(), "fake-rsync")
	if err := os.WriteFile(fakeRsync, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.RsyncBinary = fakeRsync
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.UploadDir(context.Background(), src, "/opt/pilot/services/blog/releases/0042-abc1234"); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(s.args(t), " ")
	if !strings.Contains(joined, "chmod -R u=rwX,go=rX") {
		t.Errorf("normalize should run after an rsync transfer: %v", joined)
	}
}

func TestUploadDirRejectsNonDirectory(t *testing.T) {
	s := newStub(t, 0, "", "")
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.client(t).UploadDir(context.Background(), f, "/opt/pilot/x"); err == nil {
		t.Error("want an error when the source is not a directory")
	}
}

func TestNewRequiresAddress(t *testing.T) {
	if _, err := New(Config{Name: "web-1"}); err == nil {
		t.Error("want an error for a host with no address")
	}
}
