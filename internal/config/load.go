package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

const (
	FleetFile   = "fleet.yaml"
	ServicesDir = "services"

	DefaultKeepReleases = 5
	DefaultCaddyAdmin   = "http://127.0.0.1:2019"
	DefaultSnippetDir   = "/etc/caddy/pilot.d"
	DefaultCaddyfile    = "/etc/caddy/Caddyfile"
	DefaultIndex        = "index.html"

	DefaultHealthTimeout  = Duration(60_000_000_000) // 60s
	DefaultHealthInterval = Duration(3_000_000_000)  // 3s
)

// unmarshalStrict decodes YAML and rejects unknown fields. Typos in a config
// file are the single most common way to be confused for an hour, so we'd
// rather fail on `upstrem: 8080` than silently ignore it.
func unmarshalStrict(b []byte, v any) error {
	return yaml.UnmarshalWithOptions(b, v, yaml.DisallowUnknownField())
}

// UnmarshalStrict decodes YAML into v, rejecting unknown fields. Exported so
// the agent parses its cached configuration exactly as the CLI wrote it.
func UnmarshalStrict(b []byte, v any) error { return unmarshalStrict(b, v) }

// ParseService decodes one service definition.
//
// The agent uses this to read back the specs the CLI cached on the host, so a
// definition is parsed by exactly the same code on both sides — there is no
// second, subtly different reader to drift out of sync.
func ParseService(b []byte, file string) (*Service, error) {
	s := &Service{File: file}
	if err := unmarshalStrict(b, s); err != nil {
		return nil, fmt.Errorf("%s: %s", file, formatYAMLError(err))
	}
	s.File = file
	if s.Manage == "" {
		s.Manage = ManageDeploy
	}
	if s.KeepReleases == 0 {
		s.KeepReleases = DefaultKeepReleases
	}
	if s.Compose != nil && s.Compose.Project == "" {
		s.Compose.Project = s.Name
	}
	if s.Expose != nil && s.Expose.Static != nil && s.Expose.Static.Index == "" {
		s.Expose.Static.Index = DefaultIndex
	}
	applyHealthDefaults(s, nil)
	applyRolloutDefaults(s, nil)
	return s, nil
}

// MarshalService renders a service definition for caching on a host.
func MarshalService(s *Service) ([]byte, error) {
	return yaml.Marshal(s)
}

// Load reads fleet.yaml and every services/*.yaml under root, applies defaults,
// and validates the result.
//
// A parse failure in one service file is reported as a Diagnostic and that
// service is skipped, so a single typo doesn't hide problems elsewhere. Only an
// unreadable or unparseable fleet.yaml is a hard error.
func Load(root string) (*Fleet, Diagnostics, error) {
	var ds Diagnostics

	fleetPath := filepath.Join(root, FleetFile)
	raw, err := os.ReadFile(fleetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("no %s in %s: run `pilot init` to create one", FleetFile, root)
		}
		return nil, nil, err
	}

	f := &Fleet{Root: root, File: FleetFile}
	if err := unmarshalStrict(raw, f); err != nil {
		return nil, nil, fmt.Errorf("%s: %s", FleetFile, formatYAMLError(err))
	}
	f.Root = root
	f.File = FleetFile

	for name, h := range f.Hosts {
		if h == nil {
			ds.Errorf(FleetFile, "hosts."+name, "host has no definition")
			delete(f.Hosts, name)
			continue
		}
		h.Name = name
	}

	f.Services = map[string]*Service{}
	svcDir := filepath.Join(root, ServicesDir)
	files, err := serviceFiles(svcDir)
	if err != nil {
		return nil, nil, err
	}
	for _, path := range files {
		rel := filepath.ToSlash(filepath.Join(ServicesDir, filepath.Base(path)))
		raw, err := os.ReadFile(path)
		if err != nil {
			ds.Errorf(rel, "", "cannot read: %v", err)
			continue
		}
		s := &Service{File: rel}
		if err := unmarshalStrict(raw, s); err != nil {
			ds.Errorf(rel, "", "%s", formatYAMLError(err))
			continue
		}
		s.File = rel

		base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		switch {
		case s.Name == "":
			s.Name = base
		case s.Name != base:
			ds.WarnHint(rel, "name",
				fmt.Sprintf("name %q does not match filename %q", s.Name, base),
				"rename the file to "+s.Name+".yaml so it's findable")
		}
		if prev, dup := f.Services[s.Name]; dup {
			ds.Errorf(rel, "name", "service %q is already defined in %s", s.Name, prev.File)
			continue
		}
		f.Services[s.Name] = s
	}

	applyDefaults(f)
	ds = append(ds, Validate(f)...)
	return f, ds, nil
}

func serviceFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // a fleet with no services yet is valid
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".yaml", ".yml":
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// applyDefaults fills in every value the rest of Pilot is allowed to assume is
// set. Nothing downstream of Load should have to test for a zero value.
func applyDefaults(f *Fleet) {
	if f.Caddy.Admin == "" {
		f.Caddy.Admin = DefaultCaddyAdmin
	}
	if f.Caddy.SnippetDir == "" {
		f.Caddy.SnippetDir = DefaultSnippetDir
	}
	if f.Caddy.Caddyfile == "" {
		f.Caddy.Caddyfile = DefaultCaddyfile
	}
	if f.Defaults.KeepReleases == 0 {
		f.Defaults.KeepReleases = DefaultKeepReleases
	}

	for _, h := range f.Hosts {
		if h.User == "" {
			h.User = f.Defaults.User
		}
		if h.Port == 0 {
			h.Port = f.Defaults.Port
		}
	}

	for _, s := range f.Services {
		if s.Manage == "" {
			s.Manage = ManageDeploy
		}
		if s.KeepReleases == 0 {
			s.KeepReleases = f.Defaults.KeepReleases
		}
		if s.Compose != nil && s.Compose.Project == "" {
			s.Compose.Project = s.Name
		}
		if s.Expose != nil {
			if s.Expose.Static != nil && s.Expose.Static.Index == "" {
				s.Expose.Static.Index = DefaultIndex
			}
		}
		applyHealthDefaults(s, f.Defaults.Health)
		applyRolloutDefaults(s, f.Defaults.Rollout)
	}
}

func applyHealthDefaults(s *Service, def *Health) {
	if s.Health == nil {
		if def == nil {
			return
		}
		clone := *def
		s.Health = &clone
	}
	h := s.Health
	if h.Timeout.IsZero() {
		if def != nil && !def.Timeout.IsZero() {
			h.Timeout = def.Timeout
		} else {
			h.Timeout = DefaultHealthTimeout
		}
	}
	if h.Interval.IsZero() {
		if def != nil && !def.Interval.IsZero() {
			h.Interval = def.Interval
		} else {
			h.Interval = DefaultHealthInterval
		}
	}
	if h.HTTP != nil && h.HTTP.Expect == 0 {
		h.HTTP.Expect = 200
	}
}

func applyRolloutDefaults(s *Service, def *Rollout) {
	if s.Rollout == nil {
		if def != nil {
			clone := *def
			s.Rollout = &clone
		} else {
			s.Rollout = &Rollout{}
		}
	}
	r := s.Rollout
	if r.Strategy == "" {
		r.Strategy = StrategyRecreate
	}
	if r.Concurrency == 0 {
		r.Concurrency = 1
	}
	// Drain is deliberately *not* defaulted here. Leaving it zero preserves
	// "the operator did not set this", which validation needs in order to warn
	// that both versions will serve traffic for that window. Consumers call
	// DrainOrDefault instead.
}

// formatYAMLError renders a decode failure with the offending source line,
// which is most of the value of using goccy over the standard decoder.
func formatYAMLError(err error) string {
	if s := yaml.FormatError(err, false, true); s != "" {
		return strings.TrimSpace(s)
	}
	return err.Error()
}

// ResolveTargets expands a selector into services. A selector is either a
// service name, or "@tag" matching every service that runs on a tagged host.
func (f *Fleet) ResolveTargets(selector string) ([]*Service, error) {
	if tag, ok := strings.CutPrefix(selector, "@"); ok {
		var out []*Service
		for _, name := range f.ServiceNames() {
			s := f.Services[name]
			for _, hn := range s.Hosts {
				if h, ok := f.Hosts[hn]; ok && h.HasTag(tag) {
					out = append(out, s)
					break
				}
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no services run on hosts tagged %q", tag)
		}
		return out, nil
	}
	s, ok := f.Services[selector]
	if !ok {
		return nil, fmt.Errorf("no such service %q", selector)
	}
	return []*Service{s}, nil
}

// ServiceNames returns service names in sorted order.
func (f *Fleet) ServiceNames() []string {
	out := make([]string, 0, len(f.Services))
	for n := range f.Services {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// HostNames returns host names in sorted order.
func (f *Fleet) HostNames() []string {
	out := make([]string, 0, len(f.Hosts))
	for n := range f.Hosts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
