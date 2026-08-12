// Package local implements transport.Executor against the machine the process
// is already running on.
//
// This is what the agent uses. Because it satisfies the same interface as the
// SSH client, every runtime adapter, the Caddy operations, and the release
// machinery work unchanged inside the daemon — none of them ever had to know
// whether their commands crossed a network.
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// Executor runs commands on the local host.
type Executor struct {
	// Shell is the interpreter used for commands and scripts.
	Shell string
	// Host names this machine in messages.
	Host string
}

// New returns an executor for this machine.
func New(host string) *Executor {
	if host == "" {
		host, _ = os.Hostname()
	}
	return &Executor{Shell: "/bin/sh", Host: host}
}

func (e *Executor) shell() string {
	if e.Shell != "" {
		return e.Shell
	}
	return "/bin/sh"
}

func (e *Executor) Label() string {
	if e.Host != "" {
		return e.Host
	}
	return "localhost"
}

// Run executes a command line through the shell.
//
// The shell is used rather than exec'ing directly because callers hand us
// already-quoted command lines containing pipes and redirections — the same
// strings that go over SSH.
func (e *Executor) Run(ctx context.Context, cmd string) (transport.Result, error) {
	return e.run(ctx, []string{"-c", cmd}, nil)
}

// RunScript pipes a script to the shell's stdin.
func (e *Executor) RunScript(ctx context.Context, body string) (transport.Result, error) {
	return e.run(ctx, []string{"-s"}, strings.NewReader(transport.Script(body)))
}

func (e *Executor) run(ctx context.Context, args []string, stdin io.Reader) (transport.Result, error) {
	var stdout, stderr bytes.Buffer
	code, err := runCommand(ctx, e.shell(), args, stdin, &stdout, &stderr)
	return transport.Result{
		ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String(),
	}, err
}

// RunInput runs a command with stdin supplied.
func (e *Executor) RunInput(ctx context.Context, cmd string, stdin []byte) (transport.Result, error) {
	return e.run(ctx, []string{"-c", cmd}, bytes.NewReader(stdin))
}

// Stream runs a command, forwarding output as it arrives.
func (e *Executor) Stream(ctx context.Context, cmd string, stdout, stderr io.Writer) (int, error) {
	return runCommand(ctx, e.shell(), []string{"-c", cmd}, nil, stdout, stderr)
}

func (e *Executor) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

// WriteFile writes atomically, so a reader never sees a partial file and an
// interrupted write leaves the previous version intact.
func (e *Executor) WriteFile(ctx context.Context, path string, data []byte, mode string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	perm := os.FileMode(0o644)
	if mode != "" {
		n, err := strconv.ParseUint(strings.TrimPrefix(mode, "0"), 8, 32)
		if err != nil {
			return fmt.Errorf("invalid mode %q: %w", mode, err)
		}
		perm = os.FileMode(n)
	}

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (e *Executor) Exists(ctx context.Context, path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (e *Executor) HasCommand(ctx context.Context, name string) (bool, error) {
	_, err := exec.LookPath(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return false, nil
	}
	return false, nil
}

func (e *Executor) MkdirAll(ctx context.Context, dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// RemoveAll deletes a path, with the same guard the SSH executor applies.
//
// The agent runs as root, so a path assembled from an empty variable must not
// be allowed to expand into something catastrophic.
func (e *Executor) RemoveAll(ctx context.Context, path string) error {
	clean := filepath.Clean(path)
	if clean == "" || clean == "/" || clean == "." || !strings.HasPrefix(clean, "/") {
		return fmt.Errorf("refusing to remove %q: expected an absolute path below /", path)
	}
	if strings.Count(clean, "/") < 2 {
		return fmt.Errorf("refusing to remove %q: too close to the filesystem root", clean)
	}
	return os.RemoveAll(clean)
}

// UploadDir copies a tree. On the agent both sides are local, so this is a
// plain recursive copy rather than a transfer.
//
// Modes are normalized rather than preserved, matching the SSH executor: the
// source is a staging directory whose permissions describe the builder, not
// the release. A 0700 directory would be unreadable by the process that has to
// serve it — Caddy's file_server runs as its own user — so directories become
// 0755 and files 0644, keeping execute bits for staged binaries.
func (e *Executor) UploadDir(ctx context.Context, srcDir, dstDir string) error {
	fi, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", srcDir)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dstDir, 0o755); err != nil {
		return err
	}

	return filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(dstDir, rel)

		switch {
		case info.IsDir():
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			return os.Chmod(dst, 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			os.Remove(dst)
			return os.Symlink(target, dst)
		}

		mode := os.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, mode); err != nil {
			return err
		}
		return os.Chmod(dst, mode)
	})
}

// runCommand distinguishes "the command ran and failed" (an exit code) from
// "the command could not be run" (an error).
func runCommand(ctx context.Context, bin string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if ctx.Err() != nil {
			return ee.ExitCode(), ctx.Err()
		}
		return ee.ExitCode(), nil
	}
	return 0, err
}

// Swap performs the atomic release swap in-process.
//
// This is the same create-then-rename dance the shell form does, but through
// rename(2) directly: portable, genuinely atomic, and with no subprocess.
func (e *Executor) Swap(serviceDir, releaseID string) error {
	return release.Swap(serviceDir, releaseID)
}

// compile-time checks that this satisfies the shared interfaces.
var (
	_ transport.Executor = (*Executor)(nil)
	_ transport.Swapper  = (*Executor)(nil)
)
