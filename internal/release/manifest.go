package release

import (
	"encoding/json"
	"fmt"
	"time"
)

// ManifestSchema is bumped when the on-host format changes incompatibly.
const ManifestSchema = 1

// Manifest is a release's self-description, written into the release directory
// at stage time. It is the contract that makes a six-month-old release still
// intelligible, and it is what drift detection compares live state against.
//
// It deliberately records environment variable *names* and a digest of their
// values, never the values themselves. The manifest is read by `pilot releases`
// and `pilot diff` and may end up on a terminal or in a log; resolved secrets
// must not travel with it.
type Manifest struct {
	Schema   int    `json:"schema"`
	Service  string `json:"service"`
	Release  string `json:"release"`
	Sequence int    `json:"sequence"`
	Hash     string `json:"hash"`
	Runtime  string `json:"runtime"`
	Host     string `json:"host"`

	CreatedAt  time.Time `json:"created_at"`
	DeployedBy string    `json:"deployed_by,omitempty"`

	Source    *SourceRef `json:"source,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`

	// EnvKeys lists the variable names present in .env, sorted. EnvHash covers
	// the names and values together, so a changed secret shows up as a changed
	// digest without the secret being stored.
	EnvKeys []string `json:"env_keys,omitempty"`
	EnvHash string   `json:"env_hash,omitempty"`

	// RouteHash digests the rendered Caddy snippet, empty when the service is
	// not exposed. Comparing it against the installed snippet is what lets a
	// deploy skip the Caddy reload.
	RouteHash string `json:"route_hash,omitempty"`
}

type SourceRef struct {
	Repo   string `json:"repo,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// ArtifactKind distinguishes what a recorded artifact actually is.
type ArtifactKind string

const (
	ArtifactImage ArtifactKind = "image"
	ArtifactFile  ArtifactKind = "file"
	ArtifactDir   ArtifactKind = "dir"
)

// Artifact records one shipped thing and its content digest. Image references
// are always pinned to a digest here — a tag would let the bits underneath a
// release change, which would make the release mutable in all but name.
type Artifact struct {
	Name   string       `json:"name"`
	Kind   ArtifactKind `json:"kind"`
	Ref    string       `json:"ref,omitempty"`
	Digest string       `json:"digest"`
	Size   int64        `json:"size,omitempty"`
}

// Validate checks that a manifest is internally consistent before it is written
// or after it is read back.
func (m *Manifest) Validate() error {
	if m.Schema != ManifestSchema {
		return fmt.Errorf("manifest schema %d, this build understands %d", m.Schema, ManifestSchema)
	}
	if m.Service == "" {
		return fmt.Errorf("manifest has no service")
	}
	if m.Release == "" {
		return fmt.Errorf("manifest has no release id")
	}
	seq, _, err := ParseID(m.Release)
	if err != nil {
		return err
	}
	if m.Sequence != seq {
		return fmt.Errorf("manifest sequence %d disagrees with release id %q", m.Sequence, m.Release)
	}
	for _, a := range m.Artifacts {
		if a.Digest == "" {
			return fmt.Errorf("artifact %q has no digest", a.Name)
		}
		if a.Kind == ArtifactImage && a.Ref != "" && !isDigestPinned(a.Ref) {
			return fmt.Errorf("image artifact %q is not pinned to a digest: %s", a.Name, a.Ref)
		}
	}
	return nil
}

func isDigestPinned(ref string) bool {
	for i := range len(ref) {
		if ref[i] == '@' {
			return true
		}
	}
	return false
}

// MarshalManifest renders a manifest for writing to the host. It is indented
// because someone will `cat` it at 2am.
func MarshalManifest(m *Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// UnmarshalManifest parses and validates a manifest read back from a host.
func UnmarshalManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unreadable manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
