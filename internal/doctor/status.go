package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

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
// The server is read over SSH from loopback rather than through its domain.
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
		Hint:   "pilot agent upgrade " + host + " removes it too",
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
		out = append(out, readHostView(ctx, r, f))
	}

	if drift := statusRouteDrift(ctx, r, f, paths); drift != nil {
		out = append(out, *drift)
	}
	return out
}

// readHostView reports which hosts the server says have gone quiet.
//
// It reads the server's own per-host view, on a loopback port Caddy never
// routes to, because the public page names labels and never hosts. Mapping
// labels back to hosts blamed every host sharing a label, hid a silent host
// behind a down row, and never saw a host with nothing on the page.
func readHostView(ctx context.Context, r caddy.Runner, f *config.Fleet) Finding {
	host := f.Status.Host
	url := fmt.Sprintf("http://127.0.0.1:%d%s", statuspage.LocalPort, server.HostsPath)
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
			Title: "status server's host view unreadable from its host", Detail: detail,
			Hint: "a server older than this pilot has none: pilot agent upgrade " + host +
				". The check also needs curl on " + host,
		}
	}

	var view server.HostsView
	if err := json.Unmarshal([]byte(res.Stdout), &view); err != nil {
		return Finding{
			Status: StatusWarn, Scope: ScopeHost, Host: host,
			Title: "status server's host view is not the JSON this pilot expects", Detail: firstLine(err.Error()),
		}
	}

	var silent, since []string
	for _, h := range view.Hosts {
		if !h.Silent {
			continue
		}
		silent = append(silent, h.Host)
		if h.Reported {
			since = append(since, h.Host+" last reported at "+h.LastReport.UTC().Format(time.RFC3339)+".")
		} else {
			since = append(since, h.Host+" has not reported since the server started.")
		}
	}
	if len(silent) == 0 {
		return Finding{
			Status: StatusOK, Scope: ScopeHost, Host: host,
			Title: fmt.Sprintf("status server running, %d host(s) watched, none silent", len(view.Hosts)),
		}
	}
	return Finding{
		Status: StatusFail, Scope: ScopeHost, Host: host,
		Title:  "not reporting to the status page: " + strings.Join(silent, ", "),
		Detail: strings.Join(since, " "),
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
