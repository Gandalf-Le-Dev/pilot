package caddy

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// A domain that several services share cannot be a site block in each of
// their files: Caddy rejects two blocks for one address as ambiguous, and a
// path in a site address is deprecated since Caddy 2.10. So a route on a
// shared domain renders as `handle` blocks, and Pilot installs one copy per
// domain at _site.<domain>/<service>.caddy, imported by the site block in
// _site.<domain>.caddy. That block carries only the bind and the import, so
// any service can write it and every writer writes the same bytes. Caddy
// orders the imported handles deepest path first, whichever file they came
// from, which is what lets each service keep its own file.
const (
	sitePrefix = "_site."

	// The header lines that make a rendered route shared, and carry what the
	// site block needs.
	sharedMarker     = "# shared site: "
	sharedBindMarker = "# shared site bind: "
)

// sharedRoute is what the header of a shared route records.
type sharedRoute struct {
	domains []string
	bind    []string
}

// parseShared reads the header Render writes on a shared route. A route
// without it is a site block of its own, installed at <service>.caddy.
func parseShared(snippet string) (sharedRoute, bool) {
	var sr sharedRoute
	for line := range strings.SplitSeq(snippet, "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		if v, ok := strings.CutPrefix(line, sharedMarker); ok {
			sr.domains = strings.Fields(v)
		}
		if v, ok := strings.CutPrefix(line, sharedBindMarker); ok {
			sr.bind = strings.Fields(v)
		}
	}
	return sr, len(sr.domains) > 0
}

// Shared reports whether a rendered route installs as handles inside Pilot's
// per-domain site blocks rather than as a site block of its own.
func Shared(snippet string) bool {
	_, ok := parseShared(snippet)
	return ok
}

// routeBinds reports whether a route reaches Caddy with an explicit bind.
func routeBinds(snippet string) bool {
	if sr, ok := parseShared(snippet); ok {
		return len(sr.bind) > 0
	}
	siteBind, _ := BindUsage(snippet)
	return siteBind
}

// siteName is the stem of a shared domain's files. A wildcard domain would
// otherwise put a second `*` in the import glob, which Caddy refuses.
func siteName(domain string) string {
	return sitePrefix + strings.NewReplacer("*", "_", "?", "_", "[", "_", "]", "_").Replace(domain)
}

// StatusRoute names the status page's route among the services' routes.
// Config validation reserves the leading underscore, so no service can claim
// it.
const StatusRoute = "_status"

// SiteFile is the site block of a shared domain, relative to the snippet dir.
func SiteFile(domain string) string { return siteName(domain) + ".caddy" }

// HandlesFile is a service's handles inside a shared domain's site, relative
// to the snippet dir.
func HandlesFile(domain, service string) string {
	return path.Join(siteName(domain), SnippetName(service))
}

// renderSite produces a shared domain's site block. The import is relative,
// and Caddy resolves it against this file's own directory.
func renderSite(domain string, bind []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# managed by pilot — site: %s — do not edit\n", domain)
	fmt.Fprintf(&b, "# serves the handles in %s/, one file per service routed here\n", siteName(domain))
	fmt.Fprintf(&b, "%s {\n", domain)
	if len(bind) > 0 {
		fmt.Fprintf(&b, "\tbind %s\n", strings.Join(bind, " "))
	}
	fmt.Fprintf(&b, "\timport %s/*.caddy\n}\n", siteName(domain))
	return b.String()
}

// routeFile is one file a route occupies, relative to the snippet dir.
type routeFile struct {
	rel     string
	content string

	// site marks a shared domain's site block, which belongs to every
	// service routed into it rather than to this one.
	site bool
}

// routeFiles lays a rendered route out as the files it occupies.
func routeFiles(service, snippet string) []routeFile {
	sr, ok := parseShared(snippet)
	if !ok {
		return []routeFile{{rel: SnippetName(service), content: snippet}}
	}
	var out []routeFile
	for _, d := range sr.domains {
		out = append(out,
			routeFile{rel: HandlesFile(d, service), content: snippet},
			routeFile{rel: SiteFile(d), content: renderSite(d, sr.bind), site: true})
	}
	return out
}

// staleFiles returns the files a route change removes, relative to the
// snippet dir: the service's own files its new layout no longer has, and the
// shared site block of each of the route's domains that would be left with
// no handles.
//
// A site block with nothing to import still loads and answers its domain
// with an empty 200, and beside a block of the domain's own it is a second
// declaration Caddy rejects. So it goes with its last handles, and also when
// a failed change or a hand edit left it without any.
func staleFiles(installed []string, service string, want []routeFile, domains []string) []string {
	keep := map[string]bool{}
	for _, f := range want {
		keep[f.rel] = true
	}

	var stale, dirs []string
	for _, d := range domains {
		if !slices.Contains(dirs, siteName(d)) {
			dirs = append(dirs, siteName(d))
		}
	}
	for _, rel := range installed {
		dir, file := path.Split(rel)
		if keep[rel] || file != SnippetName(service) {
			continue
		}
		stale = append(stale, rel)
		if dir = strings.TrimSuffix(dir, "/"); dir != "" && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}

	for _, dir := range dirs {
		site := dir + ".caddy"
		if keep[site] || !slices.Contains(installed, site) {
			continue
		}
		if !slices.ContainsFunc(installed, func(rel string) bool {
			return strings.HasPrefix(rel, dir+"/") && !slices.Contains(stale, rel)
		}) {
			stale = append(stale, site)
		}
	}
	return stale
}

// HeadlessSite is a handle directory that no site block imports.
type HeadlessSite struct {
	Dir      string
	Services []string
}

// siteProblems finds, among the installed route files, site blocks with no
// handles and handle directories with no site block.
func siteProblems(installed []string) (empty []string, headless []HeadlessSite) {
	for _, rel := range installed {
		dir, file := path.Split(rel)
		switch {
		case dir == "" && strings.HasPrefix(file, sitePrefix):
			stem := strings.TrimSuffix(file, ".caddy")
			if !slices.ContainsFunc(installed, func(r string) bool { return strings.HasPrefix(r, stem+"/") }) {
				empty = append(empty, rel)
			}
		case strings.HasPrefix(dir, sitePrefix):
			stem := strings.TrimSuffix(dir, "/")
			if slices.Contains(installed, stem+".caddy") {
				continue
			}
			svc := strings.TrimSuffix(file, ".caddy")
			if i := slices.IndexFunc(headless, func(h HeadlessSite) bool { return h.Dir == stem }); i >= 0 {
				headless[i].Services = append(headless[i].Services, svc)
			} else {
				headless = append(headless, HeadlessSite{Dir: stem, Services: []string{svc}})
			}
		}
	}
	return empty, headless
}

// routeDomains returns the domains a rendered route serves whole: those of a
// shared route, or the path-less addresses of a route's own site blocks.
func routeDomains(snippet string) []string {
	if sr, ok := parseShared(snippet); ok {
		return sr.domains
	}
	var out []string
	for _, addrs := range siteHeaders(snippet) {
		for _, a := range addrs {
			if !strings.Contains(a, "/") {
				out = append(out, a)
			}
		}
	}
	return out
}

// RouteState is how a service's installed route compares with a rendered one.
type RouteState int

const (
	RouteMissing RouteState = iota
	RouteDiffers
	RouteMatches
)

// Installed compares a service's route on the host with want, a route as
// Render produced it, through read, which reads one file. got is the
// installed counterpart of want's own text, for showing a difference.
//
// Every file want occupies must hold the expected bytes, the shared site
// blocks included: a handle whose site block is missing or binds elsewhere
// serves nothing. A route from before its domain was shared, still installed
// beside the new layout, counts as a difference.
func Installed(read func(string) ([]byte, error), p Paths, service, want string) (got string, state RouteState) {
	owned, same := false, true
	for _, f := range routeFiles(service, want) {
		b, err := read(path.Join(p.SnippetDir, f.rel))
		if err != nil {
			same = false
			continue
		}
		if !f.site && !owned {
			got, owned = string(b), true
		}
		if string(b) != f.content {
			same = false
		}
	}

	if Shared(want) {
		if b, err := read(path.Join(p.SnippetDir, SnippetName(service))); err == nil {
			if !owned {
				got = string(b)
			}
			owned, same = true, false
		}
	}

	switch {
	case !owned:
		return got, RouteMissing
	case same:
		return got, RouteMatches
	}
	return got, RouteDiffers
}

// SharedTexts lays a shared route out for showing a difference: every file it
// occupies, the site blocks included, under a line naming it, once as want
// has it and once as the host has it. The handles alone can match while a
// site block is missing or binds elsewhere.
func SharedTexts(read func(string) ([]byte, error), p Paths, service, want string) (wantText, gotText string) {
	var w, g strings.Builder
	for _, f := range routeFiles(service, want) {
		fmt.Fprintf(&w, "# %s\n%s", f.rel, f.content)
		if b, err := read(path.Join(p.SnippetDir, f.rel)); err == nil {
			fmt.Fprintf(&g, "# %s\n%s", f.rel, b)
		} else {
			fmt.Fprintf(&g, "# %s (missing)\n", f.rel)
		}
	}
	if b, err := read(path.Join(p.SnippetDir, SnippetName(service))); err == nil {
		fmt.Fprintf(&g, "# %s (from before the domain was shared)\n%s", SnippetName(service), b)
	}
	return w.String(), g.String()
}

// InstalledBinds reports whether a service's installed route reaches Caddy
// with an explicit bind, reading its own site block or, for a shared route,
// the site blocks of the domains its handles are under. installed is false
// when no route of the service is there.
func InstalledBinds(read func(string) ([]byte, error), p Paths, service string, domains []string) (installed, binds bool) {
	if b, err := read(path.Join(p.SnippetDir, SnippetName(service))); err == nil {
		siteBind, _ := BindUsage(string(b))
		return true, siteBind
	}
	binds = true
	for _, d := range domains {
		if _, err := read(path.Join(p.SnippetDir, HandlesFile(d, service))); err != nil {
			continue
		}
		installed = true
		b, err := read(path.Join(p.SnippetDir, SiteFile(d)))
		if siteBind, _ := BindUsage(string(b)); err != nil || !siteBind {
			binds = false
		}
	}
	return installed, installed && binds
}

// CheckSites refuses a route that would leave a domain declared twice in
// Pilot's files, before any file is touched. Caddy would reject the whole
// configuration over it, and a Caddy restart between writing the route and
// undoing it would take every site on the host down.
//
// Each service only ever changes its own files, so the remedy is always a
// deploy or a prune of the other service.
func CheckSites(ctx context.Context, r Runner, p Paths, service, snippet string) error {
	installed, err := listRoutes(ctx, r, p)
	if err != nil {
		return err
	}
	return checkSites(ctx, r, p, installed, service, snippet)
}

func checkSites(ctx context.Context, r Runner, p Paths, installed []string, service, snippet string) error {
	if sr, ok := parseShared(snippet); ok {
		args := []string{"grep", "-lxF"}
		for _, d := range sr.domains {
			if !slices.Contains(installed, SiteFile(d)) {
				args = append(args, "-e", d+" {")
			}
		}
		if len(args) == 2 {
			return nil
		}
		// grep exits 1 for no match and 2 for an error, which must not read as
		// "no conflict": an unreadable directory would let the block through.
		dir := transport.Quote(p.SnippetDir)
		res, err := r.Run(ctx, "[ -d "+dir+" ] || exit 1; cd "+dir+" || exit 2; "+
			"set -- *.caddy; [ -e \"$1\" ] || exit 1; "+transport.Join(args...)+" -- \"$@\"")
		switch {
		case err != nil:
			return err
		case res.ExitCode == 1:
			return nil
		case !res.OK():
			return fmt.Errorf("searching %s for another block of %s: %w", p.SnippetDir, strings.Join(sr.domains, ", "), res.Err())
		}
		var owners, remedies []string
		for line := range strings.SplitSeq(res.Out(), "\n") {
			file := strings.TrimSpace(line)
			if file == "" || file == SnippetName(service) || strings.HasPrefix(file, sitePrefix) {
				continue
			}
			owner := strings.TrimSuffix(file, ".caddy")
			owners = append(owners, owner)
			if owner == StatusRoute {
				remedies = append(remedies, "run `pilot doctor --fix`, which reinstalls the status page's route into the shared site")
			} else {
				remedies = append(remedies, "deploy "+owner+", which now renders as a handle in the shared site")
			}
		}
		if len(owners) > 0 {
			return fmt.Errorf("the route for %s shares %s, but the route of %s still serves the domain as a site of its own, "+
				"and Caddy would reject a second block for it\n"+
				"first %s",
				service, strings.Join(sr.domains, ", "), strings.Join(owners, ", "), strings.Join(remedies, "; then "))
		}
		return nil
	}

	for _, addrs := range siteHeaders(snippet) {
		for _, domain := range addrs {
			if strings.Contains(domain, "/") {
				continue
			}
			var others []string
			for _, rel := range installed {
				dir, file := path.Split(rel)
				if dir == siteName(domain)+"/" && file != SnippetName(service) {
					others = append(others, strings.TrimSuffix(file, ".caddy"))
				}
			}
			if len(others) > 0 {
				return fmt.Errorf("the route for %s serves %s as a site of its own, but the shared site for it still holds the routes of %s, "+
					"and Caddy would reject a second block for it\n"+
					"remove the routes of services the fleet no longer places here with `pilot routes --prune`, and redeploy those it still does; "+
					"a rollback to a release from before the domain was shared keeps the route %s has now",
					service, domain, strings.Join(others, ", "), service)
			}
		}
	}
	return nil
}
