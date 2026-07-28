// Package release owns Pilot's on-host directory layout: immutable, numbered
// release directories and the `current` symlink that selects one of them.
//
// The symlink swap is the commit point of every deploy. It is the one mechanism
// shared by all three runtimes, which is what lets `pilot rollback` mean the
// same thing for a container stack, a systemd unit, and a folder of HTML.
//
// Everything here is pure Go operating on local paths, so it is directly
// testable and is reused unchanged by the agent in phase 2.
package release

import (
	"path"
	"path/filepath"
)

// DefaultRoot is where Pilot keeps its state on a target host.
const DefaultRoot = "/opt/pilot"

// Directory and file names within a service directory.
const (
	ReleasesDir  = "releases"
	CurrentLink  = "current"
	PendingLink  = "current.tmp"
	StateFile    = "state.json"
	LockFile     = ".lock"
	ManifestFile = "manifest.json"
	EnvFile      = ".env"
	SnippetFile  = "caddy.snippet"
	ArtifactsDir = "artifacts"
)

// Layout resolves paths under a Pilot root. Paths are always slash-separated:
// they name locations on a Linux host, even when Pilot is running on macOS, so
// filepath's OS-specific separator would be wrong here.
type Layout struct {
	Root string
}

// NewLayout returns a Layout rooted at root, defaulting to DefaultRoot.
func NewLayout(root string) Layout {
	if root == "" {
		root = DefaultRoot
	}
	return Layout{Root: path.Clean(filepath.ToSlash(root))}
}

func (l Layout) Bin() string      { return path.Join(l.Root, "bin") }
func (l Layout) Agent() string    { return path.Join(l.Bin(), "pilotd") }
func (l Layout) Services() string { return path.Join(l.Root, "services") }

// Service is the per-service directory holding releases, state, and the lock.
func (l Layout) Service(name string) string { return path.Join(l.Services(), name) }

// Releases is the directory containing every retained release.
func (l Layout) Releases(name string) string { return path.Join(l.Service(name), ReleasesDir) }

// Release is one immutable release directory.
func (l Layout) Release(name, id string) string { return path.Join(l.Releases(name), id) }

// Current is the symlink naming the live release. Caddy's `root` and systemd's
// ExecStart both point through this path, so moving it is what makes a deploy
// take effect.
func (l Layout) Current(name string) string { return path.Join(l.Service(name), CurrentLink) }

// Pending is the temporary symlink used to make the swap atomic.
func (l Layout) Pending(name string) string { return path.Join(l.Service(name), PendingLink) }

func (l Layout) State(name string) string { return path.Join(l.Service(name), StateFile) }
func (l Layout) Lock(name string) string  { return path.Join(l.Service(name), LockFile) }

// Manifest is the release's self-description.
func (l Layout) Manifest(name, id string) string {
	return path.Join(l.Release(name, id), ManifestFile)
}

// Env is the rendered environment file. It holds resolved secrets and is
// written 0600 root:root.
func (l Layout) Env(name, id string) string { return path.Join(l.Release(name, id), EnvFile) }

// Snippet is the release's copy of its Caddy route, kept so that a rollback
// restores routing and code together.
func (l Layout) Snippet(name, id string) string {
	return path.Join(l.Release(name, id), SnippetFile)
}

// CurrentTarget is the symlink's value: relative to the service directory, so
// the whole tree can be moved or mounted elsewhere without breaking.
func CurrentTarget(id string) string { return path.Join(ReleasesDir, id) }
