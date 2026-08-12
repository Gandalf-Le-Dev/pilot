package doctor

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
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
		{Name: "disk", Scope: ScopeHost, NeedsNetwork: true, Run: checkDisk},
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
// what a removed service leaves behind.
func checkCaddyRoutes(ctx context.Context, env *Env) []Finding {
	var out []Finding
	paths := hostPaths(env.Fleet)

	for _, name := range env.Fleet.HostNames() {
		client := env.Client(name)
		if client == nil {
			continue
		}

		installed, err := caddy.ListSnippets(ctx, client, paths)
		if err != nil || len(installed) == 0 {
			continue
		}

		known := map[string]bool{}
		for _, s := range env.ServicesOn(name) {
			if s.Expose != nil {
				known[s.Name] = true
			}
		}

		for _, orphan := range caddy.Orphans(installed, known) {
			svc, c := orphan, client
			out = append(out, Finding{
				Status: StatusWarn, Scope: ScopeHost, Host: name,
				Title:   "orphaned route: " + caddy.SnippetName(svc),
				Detail:  "no service named " + svc + " is placed on this host",
				FixDesc: "remove it and reload caddy",
				Fix: func(ctx context.Context) error {
					_, err := caddy.RemoveSnippet(ctx, c, paths, svc)
					return err
				},
			})
		}
	}
	return out
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

		dead := false
		for _, s := range env.ServicesOn(name) {
			if s.Expose == nil {
				continue
			}
			snippet, err := client.ReadFile(ctx, path.Join(paths.SnippetDir, caddy.SnippetName(s.Name)))
			if err != nil {
				continue // not installed yet; InstallSnippet refuses at deploy time
			}
			if hasBind, _ := caddy.BindUsage(string(snippet)); hasBind {
				continue
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

		res, err := client.Run(ctx, "df -P "+transport.Quote(layout.Root)+" 2>/dev/null || df -P /")
		if err != nil || !res.OK() {
			continue
		}
		used, ok := parseDiskUsed(res.Stdout)
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

// parseDiskUsed reads the percentage column from `df -P` output.
func parseDiskUsed(out string) (int, bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, false
	}
	fields := strings.Fields(lines[len(lines)-1])
	for _, f := range fields {
		if pct, ok := strings.CutSuffix(f, "%"); ok {
			if n, err := strconv.Atoi(pct); err == nil {
				return n, true
			}
		}
	}
	return 0, false
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

	for _, svcName := range env.Fleet.ServiceNames() {
		s := env.Fleet.Services[svcName]
		if s.Expose == nil {
			continue
		}

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
