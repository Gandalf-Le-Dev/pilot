// Package deploy computes and executes the deploy pipeline:
// resolve → build → plan → stage → activate → verify, with rollback on failure.
package deploy

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/gandalfledev/pilot/internal/build"
	"github.com/gandalfledev/pilot/internal/config"
	"github.com/gandalfledev/pilot/internal/edge/caddy"
	"github.com/gandalfledev/pilot/internal/release"
	"github.com/gandalfledev/pilot/internal/runtime"
)

// RouteAction is what a deploy will do to a service's Caddy route.
type RouteAction string

const (
	RouteNone       RouteAction = "unchanged"
	RouteInstall    RouteAction = "install"
	RouteUpdate     RouteAction = "update"
	RouteNotExposed RouteAction = "none"
)

// Plan is what a deploy would do, computed before anything is touched.
type Plan struct {
	Service *config.Service `json:"-"`

	Release   string             `json:"release"`
	Hash      string             `json:"hash"`
	Commit    string             `json:"commit,omitempty"`
	Artifacts []release.Artifact `json:"artifacts,omitempty"`

	EnvKeys []string `json:"env_keys,omitempty"`
	EnvHash string   `json:"env_hash,omitempty"`

	Snippet   string `json:"-"`
	RouteHash string `json:"route_hash,omitempty"`

	Hosts []HostPlan `json:"hosts"`

	// LocalDir holds the built contents to ship; empty for image-only releases.
	LocalDir string `json:"-"`
}

// HostPlan is the part of a plan that concerns one host.
type HostPlan struct {
	Host  string      `json:"host"`
	From  string      `json:"from,omitempty"`
	To    string      `json:"to"`
	Route RouteAction `json:"route"`

	// Changes lists what differs from the release currently live there.
	Changes []Change `json:"changes,omitempty"`

	// NoOp is set when the host already runs exactly this release.
	NoOp bool `json:"noop,omitempty"`
}

// Change is one field-level difference between two releases.
type Change struct {
	Field string `json:"field"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

// Input is what Compute needs to produce a plan.
type Input struct {
	Service *config.Service
	Fleet   *config.Fleet
	Layout  release.Layout
	Build   *build.Result

	// Env is the resolved environment. Values are used to compute a digest and
	// are never stored in the plan.
	Env map[string]string

	// Targets provides a connection per host.
	Targets map[string]*runtime.Target
}

// Compute builds the plan without changing anything on any host.
func Compute(ctx context.Context, in Input) (*Plan, error) {
	s := in.Service

	if !s.Deployable() {
		return nil, fmt.Errorf("service %q is `manage: observe` and is never deployed by Pilot\n"+
			"deploy it explicitly with --force if you really mean to", s.Name)
	}

	snippet, err := renderRoute(s, in.Layout)
	if err != nil {
		return nil, err
	}

	p := &Plan{
		Service:   s,
		Commit:    in.Build.Commit,
		Artifacts: in.Build.Artifacts,
		Snippet:   snippet,
		LocalDir:  in.Build.Dir,
		EnvKeys:   sortedKeys(in.Env),
		EnvHash:   release.HashMap(in.Env),
	}
	if snippet != "" {
		p.RouteHash = release.HashBytes([]byte(snippet))
	}

	// The release hash covers everything that defines the release. Two deploys
	// with identical inputs produce the same hash, which is how "nothing
	// actually changed" becomes visible rather than being silently re-shipped.
	h := release.NewHasher()
	h.AddString("runtime", string(s.Runtime))
	h.AddString("env", p.EnvHash)
	h.AddString("route", p.RouteHash)
	for _, a := range in.Build.Artifacts {
		h.AddString("artifact:"+a.Name, a.Digest)
	}
	p.Hash = h.Sum()

	// Sequence numbers are per-service and must not collide across hosts, so
	// the plan takes the highest sequence seen anywhere in the fleet.
	seq := 1
	for _, host := range s.Hosts {
		t, ok := in.Targets[host]
		if !ok {
			continue
		}
		ids, err := runtime.ListReleases(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("listing releases on %s: %w", host, err)
		}
		if n := release.NextSeq(ids); n > seq {
			seq = n
		}
	}
	p.Release = release.FormatID(seq, p.Hash)

	for _, host := range s.Hosts {
		hp, err := planHost(ctx, in, p, host)
		if err != nil {
			return nil, err
		}
		p.Hosts = append(p.Hosts, hp)
	}
	return p, nil
}

func planHost(ctx context.Context, in Input, p *Plan, host string) (HostPlan, error) {
	hp := HostPlan{Host: host, To: p.Release, Route: RouteNotExposed}

	t, ok := in.Targets[host]
	if !ok {
		return hp, fmt.Errorf("host %s is unreachable", host)
	}

	current, err := runtime.ReadCurrent(ctx, t)
	if err != nil {
		return hp, err
	}
	hp.From = current

	if p.Snippet != "" {
		hp.Route, err = planRoute(ctx, in, p, t)
		if err != nil {
			return hp, err
		}
	}

	if current == "" {
		hp.Changes = []Change{{Field: "release", To: p.Release}}
		return hp, nil
	}

	// A manifest we cannot read is not fatal — it just means we can't show a
	// detailed diff, and the deploy proceeds.
	prev, err := runtime.ReadManifest(ctx, t, current)
	if err != nil {
		hp.Changes = []Change{{Field: "release", From: current, To: p.Release}}
		return hp, nil
	}

	hp.Changes = diff(prev, p)
	hp.NoOp = len(hp.Changes) == 0 && hp.Route == RouteNone
	return hp, nil
}

// planRoute compares the rendered snippet against what is installed.
//
// Most deploys land here with an identical snippet, which is what lets the
// executor skip the Caddy reload entirely.
func planRoute(ctx context.Context, in Input, p *Plan, t *runtime.Target) (RouteAction, error) {
	target := path.Join(in.Fleet.Caddy.SnippetDir, caddy.SnippetName(p.Service.Name))

	existing, err := t.Client.ReadFile(ctx, target)
	if err != nil {
		return RouteInstall, nil
	}
	if string(existing) == p.Snippet {
		return RouteNone, nil
	}
	return RouteUpdate, nil
}

// diff reports what changed between the live release and the planned one.
func diff(prev *release.Manifest, p *Plan) []Change {
	var out []Change

	prevArtifacts := map[string]string{}
	for _, a := range prev.Artifacts {
		prevArtifacts[a.Name] = a.Digest
	}
	for _, a := range p.Artifacts {
		if old := prevArtifacts[a.Name]; old != a.Digest {
			out = append(out, Change{Field: a.Name, From: shortDigest(old), To: shortDigest(a.Digest)})
		}
	}

	if prev.EnvHash != p.EnvHash {
		out = append(out, Change{Field: "env", From: describeEnvChange(prev.EnvKeys, p.EnvKeys)})
	}
	if prev.RouteHash != p.RouteHash {
		out = append(out, Change{Field: "route", From: "changed"})
	}
	return out
}

// describeEnvChange names added and removed variables without revealing any
// value. A changed value shows only as "~ NAME".
func describeEnvChange(before, after []string) string {
	old := map[string]bool{}
	for _, k := range before {
		old[k] = true
	}
	new := map[string]bool{}
	for _, k := range after {
		new[k] = true
	}

	var parts []string
	for _, k := range after {
		if !old[k] {
			parts = append(parts, "+"+k)
		}
	}
	for _, k := range before {
		if !new[k] {
			parts = append(parts, "-"+k)
		}
	}
	if len(parts) == 0 {
		return "values changed"
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// renderRoute produces the Caddy snippet for a service, if it is exposed.
func renderRoute(s *config.Service, layout release.Layout) (string, error) {
	if s.Expose == nil {
		return "", nil
	}
	return caddy.Render(caddy.Input{
		Service: s.Name,
		Expose:  s.Expose,
		Root:    layout.Current(s.Name),
	})
}

// Manifest builds the manifest recorded with a release on one host.
func (p *Plan) Manifest(host string) *release.Manifest {
	seq, _, _ := release.ParseID(p.Release)
	m := &release.Manifest{
		Schema:     release.ManifestSchema,
		Service:    p.Service.Name,
		Release:    p.Release,
		Sequence:   seq,
		Hash:       p.Hash,
		Runtime:    string(p.Service.Runtime),
		Host:       host,
		CreatedAt:  now(),
		DeployedBy: deployer(),
		Artifacts:  p.Artifacts,
		EnvKeys:    p.EnvKeys,
		EnvHash:    p.EnvHash,
		RouteHash:  p.RouteHash,
	}
	if p.Service.Source != nil {
		m.Source = &release.SourceRef{
			Repo:   p.Service.Source.Repo,
			Ref:    p.Service.Source.Ref,
			Commit: p.Commit,
		}
	}
	return m
}

// NoOp reports whether every host already runs exactly this release.
func (p *Plan) NoOp() bool {
	for _, h := range p.Hosts {
		if !h.NoOp {
			return false
		}
	}
	return len(p.Hosts) > 0
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// Deployer identifies who is running this command, for the deploy record.
func Deployer() string { return deployer() }

func deployer() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}
	host, err := os.Hostname()
	if err != nil {
		return user
	}
	return user + "@" + host
}
