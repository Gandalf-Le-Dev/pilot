package doctor

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
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
		{Name: "log-secrets", Scope: ScopeHost, NeedsNetwork: true, Run: checkLogSecrets},
		{Name: "caddy-import", Scope: ScopeHost, NeedsNetwork: true, Run: checkCaddyImport},
		{Name: "caddy-routes", Scope: ScopeHost, NeedsNetwork: true, Run: checkCaddyRoutes},
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

// checkEdge verifies that exposed domains resolve and that their certificates
// are current.
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

			expiry, err := certExpiry(ctx, domain)
			switch {
			case err != nil:
				f.Status = StatusWarn
				f.Detail = fmt.Sprintf("→ %s  DNS ok  TLS unavailable (%s)", hosts, firstLine(err.Error()))
			case time.Until(expiry) < 0:
				f.Status = StatusFail
				f.Detail = fmt.Sprintf("→ %s  DNS ok  TLS expired", hosts)
			case time.Until(expiry) < 14*24*time.Hour:
				f.Status = StatusWarn
				f.Detail = fmt.Sprintf("→ %s  DNS ok  TLS valid %dd (renewal window)", hosts, int(time.Until(expiry).Hours()/24))
			default:
				f.Status = StatusOK
				f.Detail = fmt.Sprintf("→ %s  DNS ok  TLS valid %dd", hosts, int(time.Until(expiry).Hours()/24))
			}
			out = append(out, f)
		}
	}
	return out
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
