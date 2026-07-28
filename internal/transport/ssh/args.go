// Package ssh drives the system `ssh` binary rather than speaking the protocol
// in-process.
//
// That choice is deliberate. Shelling out inherits ~/.ssh/config, ProxyJump,
// hardware-backed keys, agent forwarding, known_hosts policy, and every
// per-host quirk an operator has already configured — none of which we would
// get for free from a library, and all of which a small fleet depends on.
//
// The cost is that correctness lives in argument construction, so that part is
// kept pure and is tested directly.
package ssh

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// Config describes how to reach one host. Address may be a hostname, an IP, or
// a Host alias from ~/.ssh/config — Pilot does not care which, because ssh
// resolves it.
type Config struct {
	Name    string // fleet host name, used in error messages
	Address string
	User    string
	Port    int

	IdentityFile string
	ProxyJump    string // an ssh destination, already resolved from the fleet

	// Binary is the ssh executable. Overridable so tests can substitute a stub.
	Binary string
	// RsyncBinary is the local rsync, used for directory staging when both
	// ends have it.
	RsyncBinary string

	// ControlDir holds the multiplexing sockets. One connection is established
	// per host and reused, which is what keeps a fleet-wide status query to a
	// few round-trips instead of one TCP+TLS handshake per command.
	ControlDir  string
	ControlWait time.Duration

	// BatchMode makes ssh fail instead of prompting. On by default: a password
	// prompt appearing in the middle of a parallel rollout is worse than a
	// clean failure.
	BatchMode      bool
	ConnectTimeout time.Duration
	StrictHostKey  string // passed through to StrictHostKeyChecking when set

	// Sudo wraps every remote command in `sudo -n`. The -n matters: a sudo
	// that stops to ask for a password would hang a deploy with no terminal
	// to answer it, so it fails fast instead.
	Sudo bool
}

// Wrap prepares a command line for the remote shell, escalating if the host
// requires it.
func (c Config) Wrap(cmd string) string {
	if !c.Sudo {
		return cmd
	}
	return "sudo -n sh -c " + transport.Quote(cmd)
}

// ShellCommand is the remote shell that receives a script on stdin.
func (c Config) ShellCommand() string {
	if c.Sudo {
		return "sudo -n sh -s"
	}
	return "sh -s"
}

const (
	DefaultControlWait    = 60 * time.Second
	DefaultConnectTimeout = 10 * time.Second
)

func (c Config) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "ssh"
}

func (c Config) rsyncBinary() string {
	if c.RsyncBinary != "" {
		return c.RsyncBinary
	}
	return "rsync"
}

// RsyncShell renders the -e argument telling rsync to tunnel over the same
// multiplexed connection Pilot already holds open.
func (c Config) RsyncShell() string {
	return strings.Join(append([]string{c.binary()}, c.Options()...), " ")
}

// RsyncArgs builds the argument vector for staging a directory.
//
// The trailing slash on the source is load-bearing: it means "the contents of
// this directory" rather than "this directory inside the destination".
func (c Config) RsyncArgs(localDir, remoteDir string) []string {
	args := []string{
		"--archive", "--compress", "--delete",
		"--rsh", c.RsyncShell(),
	}
	if c.Sudo {
		args = append(args, "--rsync-path", "sudo -n rsync")
	}
	return append(args,
		strings.TrimSuffix(localDir, "/")+"/",
		c.Target()+":"+strings.TrimSuffix(remoteDir, "/")+"/",
	)
}

// Target is the ssh destination, e.g. "deploy@web1.example.com".
func (c Config) Target() string {
	if c.User == "" {
		return c.Address
	}
	return c.User + "@" + c.Address
}

// Label identifies the host in human-facing output.
func (c Config) Label() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Target()
}

// ControlPath is the multiplexing socket for this host.
//
// It is derived from a hash rather than the hostname because Unix socket paths
// are capped near 104 bytes, and a long user, host, and port would silently
// blow that limit — producing a confusing "unix_listener: too long" from ssh.
func (c Config) ControlPath() string {
	sum := sha256.Sum256([]byte(c.Target() + "\x00" + fmt.Sprint(c.Port) + "\x00" + c.ProxyJump))
	dir := c.ControlDir
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:12])
}

// Options returns the -o flags common to every invocation.
func (c Config) Options() []string {
	wait := c.ControlWait
	if wait == 0 {
		wait = DefaultControlWait
	}
	timeout := c.ConnectTimeout
	if timeout == 0 {
		timeout = DefaultConnectTimeout
	}

	opts := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + c.ControlPath(),
		"-o", fmt.Sprintf("ControlPersist=%d", int(wait.Seconds())),
		"-o", fmt.Sprintf("ConnectTimeout=%d", int(timeout.Seconds())),
	}
	if c.BatchMode {
		opts = append(opts, "-o", "BatchMode=yes")
	}
	if c.StrictHostKey != "" {
		opts = append(opts, "-o", "StrictHostKeyChecking="+c.StrictHostKey)
	}
	if c.Port != 0 {
		opts = append(opts, "-p", fmt.Sprint(c.Port))
	}
	if c.IdentityFile != "" {
		opts = append(opts, "-i", c.IdentityFile)
	}
	if c.ProxyJump != "" {
		opts = append(opts, "-J", c.ProxyJump)
	}
	return opts
}

// Args builds the full ssh argument vector for running a remote command.
func (c Config) Args(remoteCmd string) []string {
	args := c.Options()
	args = append(args, c.Target())
	if remoteCmd != "" {
		args = append(args, remoteCmd)
	}
	return args
}
