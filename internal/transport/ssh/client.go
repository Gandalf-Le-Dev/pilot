package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/gandalfledev/pilot/internal/transport"
)

// ExitConnectionFailed is ssh's own exit status when it cannot establish a
// connection, as opposed to a remote command that ran and failed.
const ExitConnectionFailed = 255

// Result is the outcome of one remote command. It is the shared transport type,
// aliased here so existing call sites read naturally.
type Result = transport.Result

// ConnectionError signals that ssh could not reach the host at all, which
// callers distinguish from a command that ran and failed: one means "skip this
// host", the other means "this deploy is broken".
type ConnectionError struct {
	Host   string
	Detail string
}

func (e *ConnectionError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("cannot reach %s", e.Host)
	}
	return fmt.Sprintf("cannot reach %s: %s", e.Host, e.Detail)
}

// IsUnreachable reports whether err is a connection failure.
func IsUnreachable(err error) bool {
	var ce *ConnectionError
	return errors.As(err, &ce)
}

// Client runs commands on one host over a multiplexed ssh connection.
type Client struct {
	cfg Config
}

// New returns a client for the host and prepares the control socket directory.
func New(cfg Config) (*Client, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("host %q has no address", cfg.Name)
	}
	if cfg.ControlDir != "" {
		if err := os.MkdirAll(cfg.ControlDir, 0o700); err != nil {
			return nil, fmt.Errorf("preparing ssh control directory: %w", err)
		}
	}
	return &Client{cfg: cfg}, nil
}

func (c *Client) Config() Config { return c.cfg }
func (c *Client) Label() string  { return c.cfg.Label() }

// Run executes a command and captures its output.
func (c *Client) Run(ctx context.Context, remoteCmd string) (Result, error) {
	var stdout, stderr bytes.Buffer
	code, err := c.exec(ctx, c.cfg.Args(c.cfg.Wrap(remoteCmd)), nil, &stdout, &stderr)
	res := Result{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return res, err
	}
	if code == ExitConnectionFailed && looksLikeConnectionFailure(res.Stderr) {
		return res, &ConnectionError{Host: c.cfg.Label(), Detail: firstLine(res.Stderr)}
	}
	return res, nil
}

// RunScript pipes a shell script to the remote `sh`, avoiding quoting problems
// with long multi-line command sequences.
func (c *Client) RunScript(ctx context.Context, body string) (Result, error) {
	var stdout, stderr bytes.Buffer
	args := c.cfg.Args(c.cfg.ShellCommand())
	code, err := c.exec(ctx, args, strings.NewReader(transport.Script(body)), &stdout, &stderr)
	res := Result{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return res, err
	}
	if code == ExitConnectionFailed && looksLikeConnectionFailure(res.Stderr) {
		return res, &ConnectionError{Host: c.cfg.Label(), Detail: firstLine(res.Stderr)}
	}
	return res, nil
}

// RunInput executes a command with stdin supplied.
func (c *Client) RunInput(ctx context.Context, remoteCmd string, stdin []byte) (Result, error) {
	var stdout, stderr bytes.Buffer
	code, err := c.exec(ctx, c.cfg.Args(c.cfg.Wrap(remoteCmd)), bytes.NewReader(stdin), &stdout, &stderr)
	res := Result{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return res, err
	}
	if code == ExitConnectionFailed && looksLikeConnectionFailure(res.Stderr) {
		return res, &ConnectionError{Host: c.cfg.Label(), Detail: firstLine(res.Stderr)}
	}
	return res, nil
}

// Stream runs a command and forwards its output as it arrives, for `pilot logs`
// and for deploy progress.
func (c *Client) Stream(ctx context.Context, remoteCmd string, stdout, stderr io.Writer) (int, error) {
	return c.exec(ctx, c.cfg.Args(c.cfg.Wrap(remoteCmd)), nil, stdout, stderr)
}

// WriteFile creates a remote file with the given contents and mode.
//
// The body travels on stdin rather than inside the command line, so there is no
// length limit and no quoting hazard — which matters because this is how
// rendered .env files containing resolved secrets get to the host.
// The command comes from argv and the payload owns stdin entirely.
//
// An earlier version sent the script and the payload down one stdin, expecting
// `cat` to pick up whatever followed the script. Shells read stdin in blocks,
// so `sh` would swallow part of the payload and execute it as commands — which,
// when the payload was a Caddyfile, meant lines of site config being run as
// shell. Keeping the two on separate channels makes that impossible rather than
// unlikely.
//
// It also keeps the payload out of argv, so a resolved secret never appears in
// the host's process list.
func (c *Client) WriteFile(ctx context.Context, remotePath string, data []byte, mode string) error {
	if mode == "" {
		mode = "0644"
	}
	// Write to a temporary file beside the target and rename, so a reader
	// never sees a half-written file and a failed write leaves the previous
	// version intact.
	tmp := remotePath + ".pilot-tmp"
	cmd := strings.Join([]string{
		"mkdir -p " + transport.Quote(path.Dir(remotePath)),
		"cat > " + transport.Quote(tmp),
		"chmod " + mode + " " + transport.Quote(tmp),
		"mv -f " + transport.Quote(tmp) + " " + transport.Quote(remotePath),
	}, " && ")

	var stdout, stderr bytes.Buffer
	code, err := c.exec(ctx, c.cfg.Args(c.cfg.Wrap(cmd)), bytes.NewReader(data), &stdout, &stderr)
	if err != nil {
		return err
	}
	return Result{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}.Err()
}

// ReadFile returns the contents of a remote file.
func (c *Client) ReadFile(ctx context.Context, remotePath string) ([]byte, error) {
	res, err := c.Run(ctx, "cat "+transport.Quote(remotePath))
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, fmt.Errorf("reading %s on %s: %w", remotePath, c.Label(), res.Err())
	}
	return []byte(res.Stdout), nil
}

// Exists reports whether a remote path exists.
func (c *Client) Exists(ctx context.Context, remotePath string) (bool, error) {
	res, err := c.Run(ctx, "test -e "+transport.Quote(remotePath))
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

// HasCommand reports whether a command is available on the host.
func (c *Client) HasCommand(ctx context.Context, name string) (bool, error) {
	res, err := c.Run(ctx, "command -v "+transport.Quote(name)+" >/dev/null 2>&1")
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

// Close tears down the multiplexed connection. Leaving it open is harmless —
// ControlPersist expires it — but a clean exit shouldn't leave sockets behind.
func (c *Client) Close() error {
	if c.cfg.ControlDir == "" {
		return nil
	}
	args := append(c.cfg.Options(), "-O", "exit", c.cfg.Target())
	cmd := exec.Command(c.cfg.binary(), args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	_ = cmd.Run() // a missing socket is not an error worth reporting
	return nil
}

// exec runs the ssh binary and returns the exit code. The error return is
// reserved for failures to run ssh at all, or a cancelled context.
func (c *Client) exec(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return runCommand(ctx, c.cfg.binary(), args, stdin, stdout, stderr)
}

// runCommand executes a local program, distinguishing "the program ran and
// failed" (an exit code) from "the program could not be run" (an error).
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
	if errors.Is(err, exec.ErrNotFound) {
		return 0, fmt.Errorf("%s not found in PATH", bin)
	}
	return 0, err
}

func looksLikeConnectionFailure(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, marker := range []string{
		"connection refused", "connection timed out", "no route to host",
		"could not resolve", "name or service not known", "permission denied",
		"host key verification failed", "connection closed", "operation timed out",
		"broken pipe", "network is unreachable",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(head)
}

// compile-time check that the SSH client satisfies the shared interface, so a
// runtime adapter can be driven either remotely or in-process by the agent.
var _ transport.Executor = (*Client)(nil)
