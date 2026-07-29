package config

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
)

// Validate checks a loaded Fleet and returns every problem it finds, rather
// than stopping at the first. Callers decide what to do with warnings; errors
// must block a deploy.
func Validate(f *Fleet) Diagnostics {
	var ds Diagnostics
	validateFleet(f, &ds)
	for _, name := range f.ServiceNames() {
		validateService(f, f.Services[name], &ds)
	}
	validateRouteCollisions(f, &ds)
	validatePortCollisions(f, &ds)
	validateNotifiers(f, &ds)
	validateAlertList(f, FleetFile, "alerts", f.Alerts, alert.ScopeHost, &ds)
	return ds
}

func validateFleet(f *Fleet, ds *Diagnostics) {
	switch f.Version {
	case 0:
		ds.ErrorHint(FleetFile, "version", "missing", "add `version: 1`")
	case 1:
	default:
		ds.Errorf(FleetFile, "version", "unsupported version %d (this build understands 1)", f.Version)
	}

	if len(f.Hosts) == 0 {
		ds.WarnHint(FleetFile, "hosts", "no hosts defined", "add at least one host to deploy anywhere")
	}
	for _, name := range f.HostNames() {
		h := f.Hosts[name]
		field := "hosts." + name
		if h.Address == "" {
			ds.ErrorHint(FleetFile, field+".address", "missing",
				"set a hostname or IP, or a Host alias from ~/.ssh/config")
		}
		if h.Port < 0 || h.Port > 65535 {
			ds.Errorf(FleetFile, field+".port", "port %d out of range", h.Port)
		}
		if h.SSH.ProxyJump != "" && h.SSH.ProxyJump == name {
			ds.Errorf(FleetFile, field+".ssh.proxy_jump", "host cannot proxy through itself")
		}
	}

	if f.Caddy.Admin != "" {
		if u, err := url.Parse(f.Caddy.Admin); err != nil || u.Scheme == "" || u.Host == "" {
			ds.ErrorHint(FleetFile, "caddy.admin", fmt.Sprintf("%q is not a valid URL", f.Caddy.Admin),
				"e.g. "+DefaultCaddyAdmin)
		}
	}
	if !strings.HasPrefix(f.Caddy.SnippetDir, "/") {
		ds.Errorf(FleetFile, "caddy.snippet_dir", "must be an absolute path, got %q", f.Caddy.SnippetDir)
	}
	if !strings.HasPrefix(f.Caddy.Caddyfile, "/") {
		ds.Errorf(FleetFile, "caddy.caddyfile", "must be an absolute path, got %q", f.Caddy.Caddyfile)
	}
}

func validateService(f *Fleet, s *Service, ds *Diagnostics) {
	file := s.File

	if s.Name == "" {
		ds.Errorf(file, "name", "missing")
	}

	switch s.Runtime {
	case "":
		ds.ErrorHint(file, "runtime", "missing", "one of: "+runtimeList())
	case RuntimeCompose, RuntimeSystemd, RuntimeStatic:
	default:
		ds.ErrorHint(file, "runtime", fmt.Sprintf("unknown runtime %q", s.Runtime), "one of: "+runtimeList())
	}

	switch s.Manage {
	case ManageDeploy, ManageObserve:
	default:
		ds.ErrorHint(file, "manage", fmt.Sprintf("unknown mode %q", s.Manage), "one of: deploy, observe")
	}

	if len(s.Hosts) == 0 {
		ds.ErrorHint(file, "hosts", "missing", "list at least one host from fleet.yaml")
	}
	for i, hn := range s.Hosts {
		if _, ok := f.Hosts[hn]; !ok {
			ds.ErrorHint(file, fmt.Sprintf("hosts[%d]", i),
				fmt.Sprintf("no such host %q", hn), "known hosts: "+strings.Join(f.HostNames(), ", "))
		}
	}

	validateRuntimeShape(s, ds)
	validateBuild(s, ds)
	validateExpose(s, ds)
	validateHealth(s, ds)
	validateRollout(s, ds)
	validateAlerts(f, s, ds)

	if s.KeepReleases < 1 {
		ds.Errorf(file, "keep_releases", "must be at least 1, got %d", s.KeepReleases)
	}
}

// validateRuntimeShape checks that a service carries the config block its
// runtime needs, and none that it doesn't.
func validateRuntimeShape(s *Service, ds *Diagnostics) {
	file := s.File

	// Blocks that belong to a different runtime are always a mistake — most
	// likely a copy-paste from another service file.
	type foreign struct {
		field string
		set   bool
		owner Runtime
	}
	for _, fb := range []foreign{
		{"compose", s.Compose != nil, RuntimeCompose},
		{"unit", s.Unit != nil, RuntimeSystemd},
	} {
		if fb.set && s.Runtime != "" && s.Runtime != fb.owner {
			ds.Errorf(file, fb.field, "only valid for runtime %q, but this service is %q", fb.owner, s.Runtime)
		}
	}

	if !s.Deployable() {
		// An observe-only service is never staged, so it needs nothing beyond
		// enough identity to be found and probed.
		if s.Build != nil {
			ds.WarnHint(file, "build", "ignored for `manage: observe` services",
				"remove it, or set `manage: deploy` if you meant Pilot to ship this")
		}
		if s.Source != nil {
			ds.Warnf(file, "source", "ignored for `manage: observe` services")
		}
		return
	}

	switch s.Runtime {
	case RuntimeCompose:
		if s.Compose == nil || s.Compose.File == "" {
			ds.ErrorHint(file, "compose.file", "missing",
				"path to the compose file, relative to the service repo")
		}
	case RuntimeSystemd:
		if s.Unit == nil || s.Unit.ExecStart == "" {
			ds.ErrorHint(file, "unit.exec_start", "missing",
				"the command to run, relative to the release directory")
		}
	case RuntimeStatic:
		if s.Build == nil || (s.Build.Command == "" && len(s.Build.Output) == 0) {
			ds.ErrorHint(file, "build.output", "missing",
				"a static service needs a built directory to ship")
		}
	}
}

func validateBuild(s *Service, ds *Diagnostics) {
	if s.Build == nil {
		return
	}
	file, b := s.File, s.Build

	if b.Image != "" && s.Runtime != "" && s.Runtime != RuntimeCompose {
		ds.Errorf(file, "build.image", "only the compose runtime builds images (this service is %q)", s.Runtime)
	}
	if b.Dockerfile != "" && b.Image == "" {
		ds.ErrorHint(file, "build.dockerfile", "set without build.image",
			"add `image:` to say where the built image is pushed")
	}
	if b.Command != "" && len(b.Output) == 0 {
		ds.ErrorHint(file, "build.output", "build.command is set but produces nothing",
			"list the files or directories to ship, e.g. `output: [dist/]`")
	}
	for i, o := range b.Output {
		if strings.HasPrefix(o, "/") {
			ds.ErrorHint(file, fmt.Sprintf("build.output[%d]", i),
				fmt.Sprintf("%q is absolute", o), "outputs are relative to the build directory")
		}
	}
	if b.Image != "" && strings.Contains(b.Image, ":") {
		ds.WarnHint(file, "build.image", "image includes a tag",
			"Pilot pins to a digest at build time; the tag here is ignored")
	}
}

func validateExpose(s *Service, ds *Diagnostics) {
	e := s.Expose
	if e == nil {
		return
	}
	file := s.File

	if len(e.Domains) == 0 {
		ds.ErrorHint(file, "expose.domains", "missing", "at least one domain, e.g. [api.example.com]")
	}
	for i, d := range e.Domains {
		switch {
		case d == "":
			ds.Errorf(file, fmt.Sprintf("expose.domains[%d]", i), "empty")
		case strings.Contains(d, "://"):
			ds.ErrorHint(file, fmt.Sprintf("expose.domains[%d]", i),
				fmt.Sprintf("%q includes a scheme", d), "use a bare hostname; Caddy handles TLS")
		case strings.Contains(d, "/"):
			ds.ErrorHint(file, fmt.Sprintf("expose.domains[%d]", i),
				fmt.Sprintf("%q includes a path", d), "put paths in `expose.path`")
		}
	}

	if e.Path != "" && !strings.HasPrefix(e.Path, "/") {
		ds.ErrorHint(file, "expose.path", fmt.Sprintf("%q must start with /", e.Path), "e.g. /v1/*")
	}

	proxied := e.Upstream != 0
	switch {
	case proxied && e.IsStatic():
		ds.ErrorHint(file, "expose", "both `upstream` and `static` are set",
			"a route either proxies to a port or serves files, not both")
	case !proxied && !e.IsStatic():
		ds.ErrorHint(file, "expose", "neither `upstream` nor `static` is set",
			"add `upstream: <port>` to proxy, or `static: true` to serve files")
	}

	if e.Upstream < 0 || e.Upstream > 65535 {
		ds.Errorf(file, "expose.upstream", "port %d out of range", e.Upstream)
	}

	// A static site has no process to proxy to; a process-backed service has no
	// directory Caddy should serve directly.
	if s.Runtime == RuntimeStatic && proxied {
		ds.ErrorHint(file, "expose.upstream", "a static service has no process to proxy to",
			"use `static: true`")
	}
	if s.Runtime != "" && s.Runtime != RuntimeStatic && e.IsStatic() {
		ds.ErrorHint(file, "expose.static", fmt.Sprintf("runtime %q serves from a process, not from disk", s.Runtime),
			"use `upstream: <port>`")
	}

	for i, c := range e.Allow {
		if _, _, err := net.ParseCIDR(c); err != nil {
			if net.ParseIP(c) == nil {
				ds.ErrorHint(file, fmt.Sprintf("expose.allow[%d]", i),
					fmt.Sprintf("%q is not an IP or CIDR block", c),
					"e.g. 100.64.0.0/10 for a Tailscale tailnet")
			}
		}
	}

	if e.Timeouts != nil && !proxied {
		ds.Warnf(file, "expose.timeouts", "ignored for a file server")
	}
}

func validateHealth(s *Service, ds *Diagnostics) {
	file := s.File
	h := s.Health

	if h == nil || h.Probes() == 0 {
		if s.Deployable() {
			ds.WarnHint(file, "health", "no health check defined",
				"without one, a broken deploy can't be detected or rolled back automatically")
		}
		return
	}

	if n := h.Probes(); n > 1 {
		ds.ErrorHint(file, "health", fmt.Sprintf("%d probes configured", n),
			"set exactly one of: http, tcp, exec, docker, systemd")
	}

	if h.HTTP != nil {
		if h.HTTP.URL == "" {
			ds.Errorf(file, "health.http.url", "missing")
		} else if u, err := url.Parse(h.HTTP.URL); err != nil || u.Scheme == "" || u.Host == "" {
			ds.Errorf(file, "health.http.url", "%q is not a valid URL", h.HTTP.URL)
		}
		if h.HTTP.Expect < 100 || h.HTTP.Expect > 599 {
			ds.Errorf(file, "health.http.expect", "%d is not an HTTP status code", h.HTTP.Expect)
		}
	}
	if h.TCP != nil && h.TCP.Addr == "" {
		ds.ErrorHint(file, "health.tcp.addr", "missing", "e.g. localhost:5432")
	}
	if h.Exec != nil && len(h.Exec.Cmd) == 0 {
		ds.Errorf(file, "health.exec.cmd", "missing")
	}
	if h.Docker && s.Runtime != "" && s.Runtime != RuntimeCompose {
		ds.Errorf(file, "health.docker", "only valid for the compose runtime (this service is %q)", s.Runtime)
	}
	if h.Systemd && s.Runtime != "" && s.Runtime != RuntimeSystemd {
		ds.Errorf(file, "health.systemd", "only valid for the systemd runtime (this service is %q)", s.Runtime)
	}

	if h.Timeout > 0 && h.Interval > 0 && h.Interval > h.Timeout {
		ds.ErrorHint(file, "health.interval",
			fmt.Sprintf("interval %s exceeds timeout %s", h.Interval, h.Timeout),
			"the probe would never run twice before giving up")
	}
}

func validateRollout(s *Service, ds *Diagnostics) {
	r := s.Rollout
	if r == nil {
		return
	}
	file := s.File

	switch r.Strategy {
	case StrategyRecreate:
	case StrategyBlueGreen:
		validateBlueGreen(s, ds)
	default:
		ds.ErrorHint(file, "rollout.strategy", fmt.Sprintf("unknown strategy %q", r.Strategy),
			"this build supports: "+strings.Join(AllStrategies, ", "))
	}
	if r.Concurrency < 1 {
		ds.Errorf(file, "rollout.concurrency", "must be at least 1, got %d", r.Concurrency)
	}
	if r.Concurrency > len(s.Hosts) && len(s.Hosts) > 0 {
		ds.Warnf(file, "rollout.concurrency", "%d exceeds the %d host(s) this service runs on",
			r.Concurrency, len(s.Hosts))
	}
	if r.MaxUnhealthy < 0 {
		ds.Errorf(file, "rollout.max_unhealthy", "must be zero or more, got %d", r.MaxUnhealthy)
	}
}

// validateBlueGreen checks the extra shape a two-stack rollout needs.
//
// Everything here is caught before a deploy runs, because the failure modes —
// a port collision, a missing service name — surface as an outage rather than
// an error if they are left to discover themselves at activation time.
func validateBlueGreen(s *Service, ds *Diagnostics) {
	r, file := s.Rollout, s.File

	if s.Runtime != "" && s.Runtime != RuntimeCompose {
		ds.ErrorHint(file, "rollout.strategy",
			fmt.Sprintf("blue-green needs two live stacks, which runtime %q has no notion of", s.Runtime),
			"static sites already swap atomically; use `recreate`")
		return
	}

	if s.Expose == nil {
		ds.ErrorHint(file, "rollout.strategy", "blue-green needs a route to flip",
			"add an `expose:` block, or use `recreate`")
	}

	if r.Service == "" {
		ds.ErrorHint(file, "rollout.service", "missing",
			"name the compose service that receives traffic, e.g. `service: web`")
	}

	switch len(r.Ports) {
	case 2:
		if r.Ports[0] == r.Ports[1] {
			ds.ErrorHint(file, "rollout.ports",
				fmt.Sprintf("both colors would bind %d", r.Ports[0]),
				"the two stacks must not contend for one port")
		}
		for i, p := range r.Ports {
			if p < 1 || p > 65535 {
				ds.Errorf(file, fmt.Sprintf("rollout.ports[%d]", i), "port %d out of range", p)
			}
		}
	case 0:
		ds.ErrorHint(file, "rollout.ports", "missing",
			"declare one host port per color, e.g. `ports: [18080, 18081]`")
	default:
		ds.ErrorHint(file, "rollout.ports",
			fmt.Sprintf("%d ports given, want exactly 2", len(r.Ports)),
			"one for blue, one for green")
	}

	// The container port stays in `expose.upstream`; the host ports are the
	// colors'. Reusing one for the other is the mistake worth naming.
	if s.Expose != nil && s.Expose.Upstream != 0 {
		for i, p := range r.Ports {
			if p == s.Expose.Upstream {
				ds.ErrorHint(file, fmt.Sprintf("rollout.ports[%d]", i),
					fmt.Sprintf("port %d is also `expose.upstream`", p),
					"under blue-green, `expose.upstream` is the *container* port and `rollout.ports` are the host ports Pilot publishes")
			}
		}
	}

	if r.Drain.IsZero() {
		ds.WarnHint(file, "rollout.drain", "not set",
			fmt.Sprintf("defaulting to %s; requests in flight at the flip finish against the old version", DefaultDrain))
	}
}

// validatePortCollisions catches two services claiming the same host port on
// the same machine, which would make one of them fail to start.
func validatePortCollisions(f *Fleet, ds *Diagnostics) {
	type claim struct{ service, file, field string }
	claims := map[string][]claim{}

	record := func(host string, port int, c claim) {
		if port == 0 {
			return
		}
		key := fmt.Sprintf("%s\x00%d", host, port)
		claims[key] = append(claims[key], c)
	}

	for _, name := range f.ServiceNames() {
		s := f.Services[name]
		for _, host := range s.Hosts {
			if s.Rollout.IsBlueGreen() {
				for i, p := range s.Rollout.Ports {
					record(host, p, claim{s.Name, s.File, fmt.Sprintf("rollout.ports[%d]", i)})
				}
				continue
			}
			// Under `recreate`, expose.upstream is the host port.
			if s.Expose != nil {
				record(host, s.Expose.Upstream, claim{s.Name, s.File, "expose.upstream"})
			}
		}
	}

	keys := make([]string, 0, len(claims))
	for k := range claims {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		cs := claims[k]
		if len(cs) < 2 {
			continue
		}
		host, portStr, _ := strings.Cut(k, "\x00")
		for _, c := range cs {
			var others []string
			for _, o := range cs {
				if o.service != c.service || o.field != c.field {
					others = append(others, o.service+" ("+o.field+")")
				}
			}
			ds.ErrorHint(c.file, c.field,
				fmt.Sprintf("host port %s on %q is also claimed by: %s", portStr, host, strings.Join(others, ", ")),
				"give each stack its own port")
		}
	}
}

func validateAlerts(f *Fleet, s *Service, ds *Diagnostics) {
	validateAlertList(f, s.File, "alerts", s.Alerts, alert.ScopeService, ds)
}

// validateAlertList checks a set of rules, wherever they were declared.
//
// Rules are parsed here with the same code the agent uses, so a rule that
// validates on the operator's machine cannot fail to parse on the host at 3am.
func validateAlertList(f *Fleet, file, prefix string, alerts []Alert, scope alert.Scope, ds *Diagnostics) {
	for i, a := range alerts {
		field := fmt.Sprintf("%s[%d]", prefix, i)

		expr := strings.TrimSpace(a.When)
		if expr == "" {
			ds.ErrorHint(file, field+".when", "missing",
				"a condition, e.g. `service.down` or `host.disk.free_pct < 10`")
			continue
		}

		cond, err := alert.Parse(expr)
		if err != nil {
			ds.Errorf(file, field+".when", "%v", err)
			continue
		}

		// A host-wide metric on a service, or the reverse, would silently
		// never fire — worth catching rather than leaving to be discovered.
		if cond.Metric.Scope() != scope {
			switch scope {
			case alert.ScopeService:
				ds.ErrorHint(file, field+".when",
					fmt.Sprintf("%s is a host-wide metric", cond.Metric),
					"move it to the `alerts:` block in "+FleetFile)
			case alert.ScopeHost:
				ds.ErrorHint(file, field+".when",
					fmt.Sprintf("%s is a per-service metric", cond.Metric),
					"move it to the service's own `alerts:` block")
			}
		}

		if len(a.Notify) == 0 {
			ds.WarnHint(file, field+".notify", "no notifier listed",
				"the rule will be evaluated and recorded, but nothing will be sent")
		}
		for j, n := range a.Notify {
			if _, ok := f.Notifiers[n]; !ok {
				ds.ErrorHint(file, fmt.Sprintf("%s.notify[%d]", field, j),
					fmt.Sprintf("no notifier named %q", n),
					"define it under `notifiers:` in "+FleetFile)
			}
		}
	}
}

// validateNotifiers checks each delivery target has what its type needs.
func validateNotifiers(f *Fleet, ds *Diagnostics) {
	names := make([]string, 0, len(f.Notifiers))
	for n := range f.Notifiers {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		n := f.Notifiers[name]
		field := "notifiers." + name

		if n.Type == "" {
			ds.ErrorHint(FleetFile, field+".type", "missing",
				"one of: "+strings.Join(alert.AllNotifierTypes, ", "))
			continue
		}

		switch alert.NotifierType(n.Type) {
		case alert.TypeWebhook, alert.TypeNtfy, alert.TypeSlack, alert.TypeDiscord:
			if n.Endpoint() == "" {
				ds.ErrorHint(FleetFile, field+".url", "missing",
					fmt.Sprintf("a %s notifier posts to a URL", n.Type))
			} else if u, err := url.Parse(n.Endpoint()); err != nil || u.Scheme == "" || u.Host == "" {
				ds.Errorf(FleetFile, field+".url", "%q is not a valid URL", n.Endpoint())
			}
			if len(n.Command) > 0 {
				ds.Warnf(FleetFile, field+".command", "ignored for a %s notifier", n.Type)
			}
		case alert.TypeCommand:
			if len(n.Command) == 0 {
				ds.ErrorHint(FleetFile, field+".command", "missing",
					"the argv to run; the notification arrives as JSON on its stdin")
			}
		default:
			ds.ErrorHint(FleetFile, field+".type", fmt.Sprintf("unknown type %q", n.Type),
				"one of: "+strings.Join(alert.AllNotifierTypes, ", "))
		}
	}
}

// validateRouteCollisions catches two services claiming the same Caddy site
// address on the same host, which Caddy would reject at reload time. Keying by
// host matters: the same domain on two hosts is a normal load-balanced setup,
// not a conflict.
func validateRouteCollisions(f *Fleet, ds *Diagnostics) {
	type claim struct{ service, file string }
	claims := map[string][]claim{}

	for _, name := range f.ServiceNames() {
		s := f.Services[name]
		if s.Expose == nil {
			continue
		}
		for _, host := range s.Hosts {
			for _, addr := range s.Expose.SiteAddresses() {
				key := host + "\x00" + addr
				claims[key] = append(claims[key], claim{s.Name, s.File})
			}
		}
	}

	keys := make([]string, 0, len(claims))
	for k := range claims {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		cs := claims[k]
		if len(cs) < 2 {
			continue
		}
		host, addr, _ := strings.Cut(k, "\x00")
		names := make([]string, 0, len(cs))
		for _, c := range cs {
			names = append(names, c.service)
		}
		// Report against every participant so the error is visible wherever
		// the reader happens to be looking.
		for _, c := range cs {
			others := make([]string, 0, len(names)-1)
			for _, n := range names {
				if n != c.service {
					others = append(others, n)
				}
			}
			ds.ErrorHint(c.file, "expose.domains",
				fmt.Sprintf("site address %q on host %q is also claimed by: %s",
					addr, host, strings.Join(others, ", ")),
				"give each service a distinct domain or `expose.path`")
		}
	}
}

func runtimeList() string {
	out := make([]string, 0, len(AllRuntimes))
	for _, r := range AllRuntimes {
		out = append(out, string(r))
	}
	return strings.Join(out, ", ")
}
