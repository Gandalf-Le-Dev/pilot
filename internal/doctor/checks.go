package doctor

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/server"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/ssh"
)

// Standard is the full check set, in report order.
func Standard() []Check {
	return []Check{
		{Name: "config", Scope: ScopeConfig, Run: checkConfig},
		{Name: "image-tags", Scope: ScopeConfig, Run: checkImageTags},
		{Name: "blue-green", Scope: ScopeConfig, Run: checkBlueGreen},
		{Name: "reachability", Scope: ScopeHost, NeedsNetwork: true, Run: checkReachability},
		{Name: "prerequisites", Scope: ScopeHost, NeedsNetwork: true, Run: checkPrerequisites},
		{Name: "agent", Scope: ScopeHost, NeedsNetwork: true, Run: checkAgents},
		{Name: "units", Scope: ScopeHost, NeedsNetwork: true, Run: checkUnits},
		{Name: "log-secrets", Scope: ScopeHost, NeedsNetwork: true, Run: checkLogSecrets},
		{Name: "caddy-import", Scope: ScopeHost, NeedsNetwork: true, Run: checkCaddyImport},
		{Name: "caddy-routes", Scope: ScopeHost, NeedsNetwork: true, Run: checkCaddyRoutes},
		{Name: "caddy-bind", Scope: ScopeHost, NeedsNetwork: true, Run: checkCaddyBind},
		{Name: "caddy-paths", Scope: ScopeHost, NeedsNetwork: true, Run: checkCaddyPaths},
		{Name: "disk", Scope: ScopeHost, NeedsNetwork: true, Run: checkDisk},
		{Name: "status-server", Scope: ScopeHost, NeedsNetwork: true, Run: checkStatusServer},
		{Name: "dns-tls", Scope: ScopeEdge, NeedsNetwork: true, Run: checkEdge},
	}
}

// checkConfig lifts config diagnostics into findings. The validator already did
// the work; this only translates severities.
func checkConfig(ctx context.Context, env *Env) []Finding {
	var out []Finding

	if !env.Diags.HasErrors() {
		out = append(out, Finding{
			Status: StatusOK, Scope: ScopeConfig,
			Title: fmt.Sprintf("%s valid (%s, %s)", config.FleetFile,
				plural(len(env.Fleet.Hosts), "host"), plural(len(env.Fleet.Services), "service")),
		})
	}

	for _, d := range env.Diags.Sorted() {
		status := StatusFail
		if d.Severity == config.SevWarning {
			status = StatusWarn
		}
		title := d.File
		if d.Field != "" {
			title += ": " + d.Field
		}
		out = append(out, Finding{
			Status: status, Scope: ScopeConfig,
			Title: title, Detail: d.Message, Hint: d.Hint,
		})
	}
	return out
}

// checkReachability reports whether each host answered, and is what lets later
// host checks skip quietly instead of emitting a cascade of failures that all
// mean "the box is down".
func checkReachability(ctx context.Context, env *Env) []Finding {
	var out []Finding
	for _, name := range env.Fleet.HostNames() {
		client := env.Client(name)
		if client == nil {
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title: "unreachable", Detail: "no ssh connection could be established",
				Hint: "check the address in " + config.FleetFile + ", or try `ssh " + env.Fleet.Hosts[name].Address + "` by hand",
			})
			continue
		}

		start := time.Now()
		res, err := client.Run(ctx, "true")
		switch {
		case err != nil:
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title: "unreachable", Detail: err.Error(),
			})
		case !res.OK():
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title: "ssh works but commands fail", Detail: res.Err().Error(),
			})
		default:
			out = append(out, Finding{
				Status: StatusOK, Scope: ScopeHost, Host: name,
				Title: fmt.Sprintf("reachable in %dms", time.Since(start).Milliseconds()),
			})
		}
	}
	return out
}

// checkPrerequisites verifies only what a host actually needs. A box running
// nothing but static sites has no reason to have Docker installed, and
// demanding it would be noise.
func checkPrerequisites(ctx context.Context, env *Env) []Finding {
	var out []Finding

	for _, name := range env.Fleet.HostNames() {
		client := env.Client(name)
		if client == nil {
			continue
		}

		services := env.ServicesOn(name)
		needsDocker, needsCaddy := false, false
		for _, s := range services {
			if s.Runtime == config.RuntimeCompose {
				needsDocker = true
			}
			if s.Expose != nil {
				needsCaddy = true
			}
		}

		if needsDocker {
			out = append(out, commandFinding(ctx, client, name, "docker",
				"docker is required by the compose services on this host"))
			if ok, _ := client.Run(ctx, "docker info"); !ok.OK() {
				out = append(out, Finding{
					Status: StatusFail, Scope: ScopeHost, Host: name,
					Title:  "docker socket not accessible",
					Detail: "the ssh user cannot talk to the docker daemon",
					Hint:   "add the user to the docker group, or deploy as root",
				})
			}
		}

		if needsCaddy {
			out = append(out, commandFinding(ctx, client, name, "caddy",
				"caddy is required by the exposed services on this host"))

			paths := hostPaths(env.Fleet)
			if ok, err := caddy.AdminReachable(ctx, client, paths); err == nil && !ok {
				out = append(out, Finding{
					Status: StatusWarn, Scope: ScopeHost, Host: name,
					Title:  "caddy admin API not responding",
					Detail: paths.Admin + " did not answer",
					Hint:   "Pilot can still write routes, but cannot verify a reload took effect",
				})
			} else if err == nil {
				out = append(out, Finding{
					Status: StatusOK, Scope: ScopeHost, Host: name,
					Title: "caddy admin responding",
				})
			}
		}

		// Pilot writes to /opt/pilot and /etc/caddy and drives systemd. If it
		// cannot reach root, every one of those fails with a bare "Permission
		// denied" somewhere deep in a deploy. Catching it here turns that into
		// one sentence, before anything is attempted.
		host := env.Fleet.Hosts[name]
		whoami, err := client.Run(ctx, "id -un")
		switch {
		case err != nil:
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title: "could not determine the effective user", Detail: err.Error(),
			})
		case !whoami.OK() && host.Sudo:
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title:  "passwordless sudo is not available",
				Detail: firstLine(whoami.Stderr),
				Hint:   "a deploy has no terminal to answer a password prompt — grant NOPASSWD, or drop `sudo: true`",
			})
		case whoami.Out() != "root":
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title:  "connecting as " + whoami.Out() + ", which cannot write to /opt/pilot or /etc/caddy",
				Detail: "Pilot needs root for release directories, Caddy routes, and systemd",
				Hint:   "add `sudo: true` to this host in " + config.FleetFile + ", or connect as root",
			})
		case host.Sudo:
			out = append(out, Finding{
				Status: StatusOK, Scope: ScopeHost, Host: name,
				Title: "escalates to root via passwordless sudo",
			})
		default:
			out = append(out, Finding{
				Status: StatusOK, Scope: ScopeHost, Host: name, Title: "connecting as root",
			})
		}

		// The Pilot root only needs to exist once something has been deployed.
		layout := release.NewLayout("")
		if exists, err := client.Exists(ctx, layout.Services()); err == nil && !exists && len(services) > 0 {
			out = append(out, Finding{
				Status: StatusWarn, Scope: ScopeHost, Host: name,
				Title:  "no services deployed yet",
				Detail: layout.Services() + " does not exist",
				Hint:   "this is expected before the first `pilot deploy`",
			})
		}
	}
	return out
}

func commandFinding(ctx context.Context, client *ssh.Client, host, cmd, why string) Finding {
	ok, err := client.HasCommand(ctx, cmd)
	switch {
	case err != nil:
		return Finding{Status: StatusFail, Scope: ScopeHost, Host: host,
			Title: "could not check for " + cmd, Detail: err.Error()}
	case !ok:
		return Finding{Status: StatusFail, Scope: ScopeHost, Host: host,
			Title: cmd + " not installed", Detail: why}
	}
	return Finding{Status: StatusOK, Scope: ScopeHost, Host: host, Title: cmd + " available"}
}

// checkCaddyImport verifies the one line Pilot adds outside the directory it
// owns. It is fixable, but never silently re-added: if it disappeared, that is
// worth a human noticing.
func checkCaddyImport(ctx context.Context, env *Env) []Finding {
	var out []Finding
	paths := hostPaths(env.Fleet)

	for _, name := range env.Fleet.HostNames() {
		client := env.Client(name)
		if client == nil || !hostHasExposedService(env, name) {
			continue
		}

		raw, err := client.ReadFile(ctx, paths.Caddyfile)
		if err != nil {
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title:  "cannot read " + paths.Caddyfile,
				Detail: err.Error(),
				Hint:   "Pilot needs a global Caddyfile to import its routes from",
			})
			continue
		}

		state, reason := caddy.Inspect(string(raw), paths.Caddyfile, paths.SnippetDir)
		switch state {
		case caddy.ImportPresent:
			out = append(out, Finding{
				Status: StatusOK, Scope: ScopeHost, Host: name,
				Title: paths.Caddyfile + " imports " + caddy.ImportDirective(paths.Caddyfile, paths.SnippetDir)[len("import "):],
			})
		case caddy.ImportMissing:
			c := client
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title:   "Caddyfile does not import Pilot's routes",
				Detail:  "generated routes in " + paths.SnippetDir + " are being ignored",
				FixDesc: "append `" + caddy.ImportDirective(paths.Caddyfile, paths.SnippetDir) + "`",
				Fix: func(ctx context.Context) error {
					_, err := caddy.EnsureImport(ctx, c, paths, time.Now())
					return err
				},
			})
		case caddy.ImportUnsafe:
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title:  "Caddyfile uses the brace-less single-site form",
				Detail: reason,
				Hint:   caddy.UnsafeHint,
			})
		}
	}
	return out
}

// checkCaddyRoutes finds routes on a host with no matching service, which is
// what a removed service leaves behind, and shared site blocks that a failed
// change or a hand edit left without routes, or routes without.
func checkCaddyRoutes(ctx context.Context, env *Env) []Finding {
	var out []Finding
	paths := hostPaths(env.Fleet)

	for _, name := range env.Fleet.HostNames() {
		client := env.Client(name)
		if client == nil {
			continue
		}

		installed, err := caddy.ListSnippets(ctx, client, paths)
		if err != nil {
			continue
		}
		empty, headless, err := caddy.SiteProblems(ctx, client, paths)
		if err != nil {
			continue
		}
		c := client
		out = append(out, caddyRouteFindings(name, installed, knownRoutes(env, name), empty, headless,
			func(svc string) func(context.Context) error {
				return func(ctx context.Context) error {
					_, err := caddy.RemoveSnippet(ctx, c, paths, svc)
					return err
				}
			},
			func(ctx context.Context) error {
				_, err := caddy.TidySites(ctx, c, paths)
				return err
			})...)
	}
	return out
}

// caddyRouteFindings judges a host's routes against the services placed on
// it. remove and tidy are the repairs, so the judgement stays testable.
func caddyRouteFindings(host string, installed []string, known map[string]bool, empty []string, headless []caddy.HeadlessSite,
	remove func(svc string) func(context.Context) error, tidy func(context.Context) error) []Finding {
	// Orphans come first: fixes run in finding order, and their removal must
	// land before a shared site block is rebuilt around what is left.
	var out []Finding
	for _, svc := range caddy.Orphans(installed, known) {
		out = append(out, Finding{
			Status: StatusWarn, Scope: ScopeHost, Host: host,
			Title:   "orphaned route: " + caddy.SnippetName(svc),
			Detail:  "no service named " + svc + " is placed on this host",
			FixDesc: "remove it and reload caddy",
			Fix:     remove(svc),
		})
	}
	for _, f := range empty {
		out = append(out, Finding{
			Status: StatusWarn, Scope: ScopeHost, Host: host,
			Title:   "shared site block with no routes in it: " + f,
			Detail:  "Caddy answers its domain with an empty 200, and a route that serves the whole domain is refused beside it",
			FixDesc: "remove it and reload caddy",
			Fix:     tidy,
		})
	}
	for _, h := range headless {
		// Rebuilding the site block would route services the fleet removed;
		// the orphan findings above take those away instead.
		if !slices.ContainsFunc(h.Services, func(s string) bool { return known[s] }) {
			continue
		}
		out = append(out, Finding{
			Status: StatusFail, Scope: ScopeHost, Host: host,
			Title:   "routes with no site block: " + h.Dir + "/",
			Detail:  "no site block imports them, so Caddy serves none of these routes",
			FixDesc: "rebuild the site block from the routes' header and reload caddy",
			Fix:     tidy,
		})
	}
	return out
}

// knownRoutes names the routes a host should carry: one per exposed service
// placed there, plus the status page's on the status host.
func knownRoutes(env *Env, host string) map[string]bool {
	known := map[string]bool{}
	for _, s := range env.ServicesOn(host) {
		if s.Expose != nil {
			known[s.Name] = true
		}
	}
	if st := env.Fleet.Status; st != nil && st.Host == host {
		known[server.SnippetName] = true
	}
	return known
}

// checkCaddyBind finds installed routes sitting in a Caddy server public
// traffic never reaches.
//
// Caddy groups sites into servers by listen address, and a socket bound to a
// specific IP wins that IP's traffic over the :443 wildcard. So on a host
// whose own Caddyfile binds sites explicitly, a generated route without the
// same bind is invisible from outside — while validating, holding a valid
// certificate, and answering every request with an empty 200. Nothing else
// surfaces that; this check exists because it once cost an afternoon.
func checkCaddyBind(ctx context.Context, env *Env) []Finding {
	var out []Finding
	paths := hostPaths(env.Fleet)

	for _, name := range env.Fleet.HostNames() {
		client := env.Client(name)
		if client == nil || !hostHasExposedService(env, name) {
			continue
		}

		raw, err := client.ReadFile(ctx, paths.Caddyfile)
		if err != nil {
			continue // caddy-import already reports an unreadable Caddyfile
		}
		siteBind, defaultBind := caddy.BindUsage(string(raw))
		if !siteBind || defaultBind {
			continue // no split servers, or default_bind already covers ours
		}

		read := func(file string) ([]byte, error) { return client.ReadFile(ctx, file) }
		dead := false
		for _, s := range env.ServicesOn(name) {
			if s.Expose == nil {
				continue
			}
			installed, binds := caddy.InstalledBinds(read, paths, s.Name, s.Expose.Domains)
			if !installed || binds {
				continue // bound, or not installed yet: InstallSnippet refuses a bind-less route
			}
			dead = true
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: name,
				Title:  "route for " + s.Name + " is unreachable",
				Detail: paths.Caddyfile + " binds sites to explicit addresses, but this route has no `bind` — it sits in a server public traffic never reaches, serving an empty 200",
				Hint:   "set hosts." + name + ".caddy.bind to the same addresses, then redeploy " + s.Name,
			})
		}
		if !dead {
			out = append(out, Finding{
				Status: StatusOK, Scope: ScopeHost, Host: name,
				Title: "routes carry the Caddyfile's explicit bind",
			})
		}
	}
	return out
}

// checkCaddyPaths finds installed routes whose path matches only itself, a
// route that deploys green, passes health and TLS, and answers every real
// request with an empty 200. It also finds routes on a shared domain still
// installed the way Pilot rendered them before Caddy 2.10 deprecated a path in
// a site address.
func checkCaddyPaths(ctx context.Context, env *Env) []Finding {
	var out []Finding
	paths := hostPaths(env.Fleet)

	for _, name := range env.Fleet.HostNames() {
		client := env.Client(name)
		if client == nil {
			continue
		}
		own := map[string]string{}
		for _, s := range env.ServicesOn(name) {
			if !env.Fleet.SharesSite(s) {
				continue
			}
			// Missing means not installed, or already handles in the shared site.
			if snippet, err := client.ReadFile(ctx, path.Join(paths.SnippetDir, caddy.SnippetName(s.Name))); err == nil {
				own[s.Name] = string(snippet)
			}
		}
		out = append(out, caddyPathFindings(name, env.ServicesOn(name), own)...)
	}
	return out
}

// caddyPathFindings judges the routes on a shared domain that a host still
// has in a file of their own, keyed by service.
func caddyPathFindings(host string, services []*config.Service, own map[string]string) []Finding {
	// A whole-domain block on a shared domain refuses the path services their
	// shared site, so they must wait for it.
	blockers := func(s *config.Service) string {
		var names []string
		for _, o := range services {
			snippet, ok := own[o.Name]
			if ok && o.Name != s.Name && o.Expose.Path == "" && len(caddy.PathAddresses(snippet)) == 0 &&
				slices.ContainsFunc(o.Expose.Domains, func(d string) bool { return slices.Contains(s.Expose.Domains, d) }) {
				names = append(names, o.Name)
			}
		}
		if len(names) == 0 {
			return ""
		}
		return strings.Join(names, ", ") + ", then "
	}

	var out []Finding
	for _, s := range services {
		snippet, ok := own[s.Name]
		if !ok {
			continue
		}
		if exact := caddy.ExactPaths(snippet); len(exact) > 0 {
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeHost, Host: host,
				Title:  "route for " + s.Name + " matches only " + strings.Join(exact, ", "),
				Detail: "a path in a Caddy site address is an exact match, so every request below it gets an empty 200",
				Hint:   "redeploy " + blockers(s) + s.Name + " to install a route that matches the whole path prefix",
			})
		} else if addrs := caddy.PathAddresses(snippet); len(addrs) > 0 {
			out = append(out, Finding{
				Status: StatusWarn, Scope: ScopeHost, Host: host,
				Title:  "route for " + s.Name + " puts its path in the site address " + strings.Join(addrs, ", "),
				Detail: "Caddy 2.10 deprecates a path in a site address and logs a warning for each one on every reload",
				Hint:   "redeploy " + blockers(s) + s.Name + " to move it into a handle in the domain's shared site, " + caddy.SiteFile(s.Expose.Domains[0]),
			})
		} else if s.Expose.Path == "" {
			out = append(out, Finding{
				Status: StatusWarn, Scope: ScopeHost, Host: host,
				Title:  "route for " + s.Name + " serves " + strings.Join(s.Expose.Domains, ", ") + " as a site of its own",
				Detail: "another service serves a path on that domain, and its deploys are refused while this block declares the whole domain: Caddy would reject the shared site block as a second one",
				Hint:   "redeploy " + s.Name + " first, then the services with a path on its domains",
			})
		}
	}
	return out
}

// checkDisk warns before releases and images fill the volume, since that
// failure mode arrives without warning and takes everything down at once.
func checkDisk(ctx context.Context, env *Env) []Finding {
	var out []Finding
	layout := release.NewLayout("")

	for _, name := range env.Fleet.HostNames() {
		client := env.Client(name)
		if client == nil {
			continue
		}

		// The root alone, as the agent measures it: a missing root gives no
		// finding rather than the figure of whatever volume holds its parent.
		res, err := client.Run(ctx, "df -P "+transport.Quote(layout.Root))
		if err != nil || !res.OK() {
			continue
		}
		used, ok := release.ParseDiskUsed(res.Stdout)
		if !ok {
			continue
		}

		f := Finding{Scope: ScopeHost, Host: name, Title: fmt.Sprintf("disk %d%% used", used)}
		switch {
		case used >= 90:
			f.Status = StatusFail
			f.Detail = "less than 10% free; a deploy may fail partway through"
		case used >= 80:
			f.Status = StatusWarn
			f.Detail = "less than 20% free"
			f.Hint = "lower `keep_releases`, or prune unused docker images"
		default:
			f.Status = StatusOK
		}
		out = append(out, f)
	}
	return out
}

// checkEdge verifies that exposed domains resolve — to the right place, when
// the hosts declare a public_address — and that their certificates are current.
//
// It runs from the operator's machine rather than the host, because what
// matters is whether the site works from outside — and a first deploy of a new
// domain fails at ACME precisely when DNS is not pointed yet.
func checkEdge(ctx context.Context, env *Env) []Finding {
	var out []Finding
	var resolver net.Resolver

	for _, s := range edgeSites(env.Fleet) {
		declared := publicAddresses(env.Fleet, s.Hosts)

		for _, domain := range s.Expose.Domains {
			hosts := strings.Join(s.Hosts, ", ")
			f := Finding{Scope: ScopeEdge, Title: domain}

			addrs, err := resolver.LookupHost(ctx, domain)
			if err != nil || len(addrs) == 0 {
				f.Status = StatusFail
				f.Detail = "DNS does not resolve"
				f.Hint = "point it at " + hosts + " before deploying, or ACME issuance will fail"
				out = append(out, f)
				continue
			}

			// "DNS resolves" is all that resolution alone proves. The stronger
			// "DNS ok" is reserved for the compared case, so the report never
			// implies a relationship it has not checked — a domain can resolve
			// to a different provider entirely and still get this far.
			dnsStatus, dns := StatusOK, "DNS resolves"
			switch {
			case s.Expose.Restricted() && insideAllow(addrs, s.Expose.Allow):
				// A restricted route whose records resolve entirely inside its
				// own allow networks is private by design: DNS steers clients
				// into exactly the network the route admits, which is how a
				// tailnet-only service avoids advertising itself. That is a
				// different claim than "points at the host's public face", so
				// it gets different words and public_address is not consulted
				// — declaring private addresses there just to quiet this check
				// would let a public domain mispointed into the same network
				// pass as "DNS ok".
				dns = "DNS private (inside allow)"
			case len(declared) > 0:
				matched, strays := matchAddresses(addrs, declared)
				switch {
				case matched == 0:
					f.Status = StatusFail
					f.Detail = fmt.Sprintf("resolves to %s — not to %s (public_address %s)",
						strings.Join(addrs, ", "), hosts, strings.Join(declared, ", "))
					f.Hint = "point the DNS record at " + hosts + ", or correct hosts." + s.Hosts[0] + ".public_address"
					out = append(out, f)
					continue
				case len(strays) > 0:
					dnsStatus = StatusWarn
					dns = fmt.Sprintf("DNS split (also resolves to %s)", strings.Join(strays, ", "))
				default:
					dns = "DNS ok"
				}
			}

			expiry, err := certExpiry(ctx, domain)
			var tlsStatus Status
			var tls string
			switch {
			case err != nil && s.Expose.Restricted():
				// The probe runs from wherever the operator is. A restricted
				// route legitimately refuses machines outside its allow
				// networks, so an unreachable handshake here may be the
				// restriction working rather than TLS being broken.
				tlsStatus = StatusWarn
				tls = fmt.Sprintf("TLS unreachable from here (restricted route; %s)", firstLine(err.Error()))
			case err != nil:
				tlsStatus = StatusWarn
				tls = fmt.Sprintf("TLS unavailable (%s)", firstLine(err.Error()))
			case time.Until(expiry) < 0:
				tlsStatus = StatusFail
				tls = "TLS expired"
			case time.Until(expiry) < 14*24*time.Hour:
				tlsStatus = StatusWarn
				tls = fmt.Sprintf("TLS valid %dd (renewal window)", int(time.Until(expiry).Hours()/24))
			default:
				tlsStatus = StatusOK
				tls = fmt.Sprintf("TLS valid %dd", int(time.Until(expiry).Hours()/24))
			}

			f.Status = max(dnsStatus, tlsStatus)
			f.Detail = fmt.Sprintf("→ %s  %s  %s", hosts, dns, tls)
			out = append(out, f)
		}
	}
	return out
}

// edgeSites lists everything with a public domain: each exposed service, and
// the status page, which is a site on its host like any other and fails the
// same ways — a record never pointed, a certificate never issued.
func edgeSites(f *config.Fleet) []*config.Service {
	var out []*config.Service
	for _, name := range f.ServiceNames() {
		if s := f.Services[name]; s.Expose != nil {
			out = append(out, s)
		}
	}
	if st := f.Status; st != nil && st.Domain != "" {
		out = append(out, &config.Service{
			Name:   server.SnippetName,
			Hosts:  []string{st.Host},
			Expose: &config.Expose{Domains: []string{st.Domain}},
		})
	}
	return out
}

// publicAddresses collects every public_address declared by the named hosts.
// A service spread across hosts legitimately resolves to any of them.
func publicAddresses(f *config.Fleet, hostNames []string) []string {
	var out []string
	for _, hn := range hostNames {
		if h, ok := f.Hosts[hn]; ok {
			out = append(out, h.PublicAddress...)
		}
	}
	return out
}

// insideAllow reports whether every resolved address falls inside one of the
// route's allow networks — the shape of a deliberately private service, where
// the DNS record and the allow list are two halves of one decision.
//
// Containment must be total. A domain half inside the allow networks and half
// outside serves some clients from somewhere the route would refuse, which
// means either the DNS or the allow list is wrong — so a partial fit falls
// through to the ordinary public_address comparison and gets flagged there.
func insideAllow(resolved, allow []string) bool {
	if len(resolved) == 0 || len(allow) == 0 {
		return false
	}

	var nets []netip.Prefix
	for _, a := range allow {
		if p, err := netip.ParsePrefix(a); err == nil {
			nets = append(nets, p.Masked())
			continue
		}
		// `remote_ip` accepts bare IPs too; a single address is a full-length
		// prefix. An entry that parses as neither admits nothing — validation
		// has already flagged it.
		if ip, err := netip.ParseAddr(a); err == nil {
			nets = append(nets, netip.PrefixFrom(ip, ip.BitLen()))
		}
	}

	for _, r := range resolved {
		ip, err := netip.ParseAddr(r)
		if err != nil {
			return false
		}
		if !slices.ContainsFunc(nets, func(p netip.Prefix) bool { return p.Contains(ip) }) {
			return false
		}
	}
	return true
}

// matchAddresses compares where a domain resolves against the addresses its
// hosts declare. It counts resolved addresses found in the declared set and
// returns the strays — addresses pointing somewhere else entirely, which
// serve some clients from the wrong machine even when others match (a stale
// AAAA record does exactly this).
func matchAddresses(resolved, declared []string) (matched int, strays []string) {
	canon := func(s string) string {
		if ip := net.ParseIP(s); ip != nil {
			return ip.String()
		}
		return s
	}

	want := map[string]bool{}
	for _, d := range declared {
		want[canon(d)] = true
	}
	for _, r := range resolved {
		if want[canon(r)] {
			matched++
		} else {
			strays = append(strays, r)
		}
	}
	return matched, strays
}

func certExpiry(ctx context.Context, domain string) (time.Time, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort(domain, "443"), &tls.Config{ServerName: domain})
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return time.Time{}, fmt.Errorf("no certificate presented")
	}
	return certs[0].NotAfter, nil
}

func hostPaths(f *config.Fleet) caddy.Paths {
	return caddy.Paths{
		Caddyfile:  f.Caddy.Caddyfile,
		SnippetDir: f.Caddy.SnippetDir,
		Admin:      f.Caddy.Admin,
	}
}

func hostHasExposedService(env *Env, host string) bool {
	for _, s := range env.ServicesOn(host) {
		if s.Expose != nil {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return head
}
