package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/agent/install"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/server"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// checkStatusServer covers the status page from its own host: the server
// running, every host reporting to it, and its route as Pilot renders it.
//
// The page is read over SSH from loopback rather than through its domain.
// That separates "the server knows a host has gone quiet" from "the public
// cannot reach the page", which the edge check covers on its own.
//
// Every other host is asked whether it still runs a server, including when
// the fleet has no status block at all: a server left behind by a move keeps
// its old configuration and pages about hosts that no longer report to it.
func checkStatusServer(ctx context.Context, env *Env) []Finding {
	var out []Finding
	st := env.Fleet.Status
	for _, host := range env.Fleet.HostNames() {
		c := env.Client(host)
		if c == nil {
			continue // unreachable hosts are already reported once
		}
		if st != nil && host == st.Host {
			out = append(out, inspectStatusServer(ctx, c, env.Fleet, hostPaths(env.Fleet))...)
			continue
		}
		if f := strayStatusServer(ctx, c, env.Fleet, host); f != nil {
			out = append(out, *f)
		}
	}
	return out
}

// strayStatusServer reports a status server on a host that should not run
// one. A failed unit counts too: it is a crash loop waiting for its config.
func strayStatusServer(ctx context.Context, r caddy.Runner, f *config.Fleet, host string) *Finding {
	res, err := r.Run(ctx, "systemctl is-active pilot-server.service")
	if err != nil {
		return nil
	}
	state := strings.TrimSpace(res.Stdout)
	switch state {
	case "", "inactive", "unknown":
		return nil
	}

	why := "the fleet has no status page"
	if f.Status != nil {
		why = "status.host is " + f.Status.Host
	}
	return &Finding{
		Status: StatusFail, Scope: ScopeHost, Host: host,
		Title:  fmt.Sprintf("a status server is %s here, but %s", state, why),
		Detail: "It keeps the configuration it was last given, and alerts about every host that no longer reports to it.",
		Hint:   "pilot agent upgrade --force " + host + " removes it too",
		Fix: func(ctx context.Context) error {
			_, err := install.RemoveServer(ctx, r)
			return err
		},
		FixDesc: "stop the status server and remove its unit and configuration",
	}
}

func inspectStatusServer(ctx context.Context, r caddy.Runner, f *config.Fleet, paths caddy.Paths) []Finding {
	host := f.Status.Host
	var out []Finding

	res, err := r.Run(ctx, "systemctl is-active pilot-server.service")
	switch state := strings.TrimSpace(res.Stdout); {
	case err != nil:
		out = append(out, Finding{
			Status: StatusWarn, Scope: ScopeHost, Host: host,
			Title: "status server state unknown", Detail: firstLine(err.Error()),
		})
	case state != "active":
		out = append(out, Finding{
			Status: StatusFail, Scope: ScopeHost, Host: host,
			Title:  "status server is " + orDefault(state, "not running"),
			Detail: "The page is down, and no host going quiet will raise an alert.",
			Hint:   "pilot agent upgrade " + host + ", then: journalctl -u pilot-server -n 50 --no-pager",
		})
	default:
		out = append(out, readStatusPage(ctx, r, f))
	}

	if drift := statusRouteDrift(ctx, r, f, paths); drift != nil {
		out = append(out, *drift)
	}
	return out
}

// readStatusPage reports which hosts the server says have gone quiet.
//
// The page names services by label and never names a host, so the hosts are
// recovered from the fleet: every host that runs a service under a silent
// label. Doctor runs on the operator's machine, which may know the topology;
// the page may not.
func readStatusPage(ctx context.Context, r caddy.Runner, f *config.Fleet) Finding {
	host := f.Status.Host
	url := fmt.Sprintf("http://127.0.0.1:%d/status.json", statuspage.PublicPort)
	res, err := r.Run(ctx, transport.Join("curl", "-fsS", "--max-time", "5", url))
	if err != nil || !res.OK() {
		detail := ""
		if err != nil {
			detail = firstLine(err.Error())
		} else {
			detail = firstLine(res.Stderr)
		}
		return Finding{
			Status: StatusWarn, Scope: ScopeHost, Host: host,
			Title: "status page unreadable from its host", Detail: detail,
			Hint: "the check needs curl on " + host,
		}
	}

	var page statuspage.Page
	if err := json.Unmarshal([]byte(res.Stdout), &page); err != nil {
		return Finding{
			Status: StatusWarn, Scope: ScopeHost, Host: host,
			Title: "status page returned something other than its JSON", Detail: firstLine(err.Error()),
		}
	}

	byLabel := map[string][]string{}
	for _, h := range f.HostNames() {
		for _, name := range f.StatusServices() {
			if slices.Contains(f.Services[name].Hosts, h) {
				label := f.Status.Label(name)
				byLabel[label] = append(byLabel[label], h)
			}
		}
	}

	quiet := map[string]bool{}
	var labels []string
	for _, s := range page.Services {
		if s.State != statuspage.NotReporting {
			continue
		}
		labels = append(labels, s.Label)
		for _, h := range byLabel[s.Label] {
			quiet[h] = true
		}
	}
	if len(labels) == 0 {
		return Finding{
			Status: StatusOK, Scope: ScopeHost, Host: host,
			Title: fmt.Sprintf("status server running, %d service(s) on the page, none silent", len(page.Services)),
		}
	}

	hosts := make([]string, 0, len(quiet))
	for h := range quiet {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return Finding{
		Status: StatusFail, Scope: ScopeHost, Host: host,
		Title:  "not reporting to the status page: " + strings.Join(hosts, ", "),
		Detail: "The page shows " + strings.Join(labels, ", ") + " as not reporting.",
		Hint:   "check each host's agent (journalctl -u pilotd) and its tailnet connection",
	}
}

// statusRouteDrift compares the installed _status.caddy against what Pilot
// renders now. Repairing it is installing it again, which validates before
// reloading and puts the old file back if Caddy refuses the new one.
func statusRouteDrift(ctx context.Context, r caddy.Runner, f *config.Fleet, paths caddy.Paths) *Finding {
	host := f.Status.Host
	want, err := server.Route(f)
	if err != nil {
		return nil
	}
	file := caddy.SnippetName(server.SnippetName)
	got, err := r.ReadFile(ctx, path.Join(paths.SnippetDir, file))

	var title string
	switch {
	case err != nil:
		title = "status page route missing: " + file
	case string(got) != want:
		title = "status page route differs from what Pilot renders: " + file
	default:
		return nil
	}
	return &Finding{
		Status: StatusWarn, Scope: ScopeHost, Host: host,
		Title:   title,
		Detail:  "Caddy is not serving " + f.Status.Domain + " the way fleet.yaml says.",
		FixDesc: "install the rendered route and reload caddy",
		Fix: func(ctx context.Context) error {
			_, err := caddy.InstallSnippet(ctx, r, paths, server.SnippetName, want)
			return err
		},
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
