package config

import (
	"errors"
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

	// ServiceFile is the definition inside a per-service directory.
	ServiceFile = "service.yaml"

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

	var stray *StrayDirsError
	switch {
	case errors.As(err, &stray):
		for _, d := range stray.Dirs {
			ds.ErrorHint(filepath.ToSlash(filepath.Join(ServicesDir, d)), "",
				fmt.Sprintf("directory has no %s, so no service is defined here", ServiceFile),
				"add "+ServiceFile+", or remove the directory if it is left over")
		}
	case err != nil:
		return nil, nil, err
	}

	for _, sf := range files {
		rel := sf.rel
		raw, err := os.ReadFile(sf.path)
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

		// The definition sits beside what it deploys, so that directory is the
		// source unless the service says otherwise. This is the whole reason the
		// layout is worth changing: it deletes a line of pointing-elsewhere from
		// every service.
		// Not for `manage: observe`: Pilot never deploys those, so handing them a
		// source would earn them a warning for a field they did not write. The
		// validator found this the moment the example was loaded.
		if sf.Own && s.Deployable() && (s.Source == nil || (s.Source.Repo == "" && s.Source.Path == "")) {
			if s.Source == nil {
				s.Source = &Source{}
			}
			s.Source.Path = sf.dir
		}

		switch {
		case s.Name == "":
			s.Name = sf.name
		case s.Name != sf.name && sf.Own:
			ds.WarnHint(rel, "name",
				fmt.Sprintf("name %q does not match directory %q", s.Name, sf.name),
				"rename the directory to services/"+s.Name+" so it's findable")
		case s.Name != sf.name:
			ds.WarnHint(rel, "name",
				fmt.Sprintf("name %q does not match filename %q", s.Name, sf.name),
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

// serviceFile is one discovered service definition.
type serviceFile struct {
	path string // absolute path on disk
	rel  string // path for diagnostics, relative to the fleet root
	name string // the name implied by where it was found
	dir  string // directory holding the service, relative to the fleet root

	// Own is true for the directory form, where the definition sits beside the
	// files it deploys. Only then does `source.path` default to that directory.
	Own bool
}

// serviceFiles finds every service definition under dir.
//
// Two layouts, both supported. `services/wakapi.yaml` is the original flat form.
// `services/wakapi/service.yaml` keeps a service's definition beside the compose
// file it deploys, which is the layout worth having: the two are halves of one
// thing, and separating them forced a `source: {path: src/wakapi}` line into
// every service that existed only to point across the gap.
func serviceFiles(dir string) ([]serviceFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // a fleet with no services yet is valid
		}
		return nil, err
	}

	var out []serviceFile
	var strays []string

	for _, e := range entries {
		if e.IsDir() {
			inner := filepath.Join(dir, e.Name(), ServiceFile)
			if _, err := os.Stat(inner); err != nil {
				// A directory here looks like a service. Losing it silently is
				// the worst outcome — the service simply stops existing — so it
				// is collected and reported rather than skipped.
				strays = append(strays, e.Name())
				continue
			}
			out = append(out, serviceFile{
				path: inner,
				rel:  filepath.ToSlash(filepath.Join(ServicesDir, e.Name(), ServiceFile)),
				name: e.Name(),
				dir:  filepath.ToSlash(filepath.Join(ServicesDir, e.Name())),
				Own:  true,
			})
			continue
		}

		switch filepath.Ext(e.Name()) {
		case ".yaml", ".yml":
			out = append(out, serviceFile{
				path: filepath.Join(dir, e.Name()),
				rel:  filepath.ToSlash(filepath.Join(ServicesDir, e.Name())),
				name: strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	sort.Strings(strays)
	return out, strayError(strays)
}

// strayError reports directories under services/ that hold no definition.
func strayError(strays []string) error {
	if len(strays) == 0 {
		return nil
	}
	return &StrayDirsError{Dirs: strays}
}

// StrayDirsError names directories under services/ with no service.yaml.
//
// Not fatal — Load turns it into diagnostics — but never silent. A half-migrated
// service whose definition is missing would otherwise vanish from the fleet, and
// the first sign of that would be `pilot status` quietly listing one fewer
// service than the operator expects.
type StrayDirsError struct{ Dirs []string }

func (e *StrayDirsError) Error() string {
	return fmt.Sprintf("no %s in services/%s", ServiceFile, strings.Join(e.Dirs, ", services/"))
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
