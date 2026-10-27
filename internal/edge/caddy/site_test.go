package caddy

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport/local"
)

const (
	sharedAPI = "# managed by pilot — service: api — do not edit\n" +
		"# shared site: example.com www.example.com\n" +
		"handle /v1 {\n\trespond api\n}\n"
	sharedSite = "# managed by pilot — service: site — do not edit\n" +
		"# shared site: example.com\n" +
		"handle {\n\trespond site\n}\n"
	legacyAPI = "example.com/v1, example.com/v1/* {\n\trespond api\n}\n"
)

func TestParseShared(t *testing.T) {
	sr, ok := parseShared("# managed by pilot — service: api — do not edit\n" +
		"# shared site: a.example.com b.example.com\n" +
		"# shared site bind: 10.0.0.1 ::1\n" +
		"handle /v1 {\n}\n" +
		"# shared site: c.example.com\n")
	if !ok {
		t.Fatal("a route with the shared-site header is shared")
	}
	if strings.Join(sr.domains, ",") != "a.example.com,b.example.com" || strings.Join(sr.bind, ",") != "10.0.0.1,::1" {
		t.Errorf("parsed %+v; only the header counts", sr)
	}
	if Shared(legacyAPI) || Shared("api.example.com {\n}\n") {
		t.Error("a site block is not a shared route")
	}
}

func TestRenderSite(t *testing.T) {
	want := `# managed by pilot — site: *.example.com — do not edit
# serves the handles in _site._.example.com/, one file per service routed here
*.example.com {
	bind 10.0.0.1 ::1
	import _site._.example.com/*.caddy
}
`
	if got := renderSite("*.example.com", []string{"10.0.0.1", "::1"}); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestInstallSharedRoute(t *testing.T) {
	f := newFake(map[string]string{testPaths.Caddyfile: "import pilot.d/*.caddy\n"})

	action, err := InstallSnippet(context.Background(), f, testPaths, "api", sharedAPI)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionInstalled {
		t.Errorf("action = %q, want %q", action, ActionInstalled)
	}
	for _, d := range []string{"example.com", "www.example.com"} {
		if got := f.files["/etc/caddy/pilot.d/_site."+d+"/api.caddy"]; got != sharedAPI {
			t.Errorf("handles for %s = %q", d, got)
		}
		if got := f.files["/etc/caddy/pilot.d/_site."+d+".caddy"]; got != renderSite(d, nil) {
			t.Errorf("site block for %s = %q", d, got)
		}
	}
	if !f.ran("caddy reload") {
		t.Error("a new route should reload")
	}

	// The second service on the domain finds the site block already right.
	f.cmds = nil
	if _, err := InstallSnippet(context.Background(), f, testPaths, "site", sharedSite); err != nil {
		t.Fatal(err)
	}
	if got := f.files["/etc/caddy/pilot.d/_site.example.com/site.caddy"]; got != sharedSite {
		t.Errorf("handles for site = %q", got)
	}

	f.cmds = nil
	if action, err := InstallSnippet(context.Background(), f, testPaths, "api", sharedAPI); err != nil || action != ActionNone {
		t.Errorf("reinstall = %q, %v; want unchanged", action, err)
	}
	if f.ran("caddy reload") {
		t.Error("an unchanged shared route must not reload")
	}
}

// A route from before its domain was shared moves in one validated reload:
// the old site block goes and the handles arrive together, and the old file
// is removed before anything is written, so no state on disk in between
// declares the domain twice.
func TestInstallSharedRouteReplacesTheOldBlock(t *testing.T) {
	f := newFake(map[string]string{
		testPaths.Caddyfile:            "import pilot.d/*.caddy\n",
		"/etc/caddy/pilot.d/api.caddy": legacyAPI,
	})

	action, err := InstallSnippet(context.Background(), f, testPaths, "api", sharedAPI)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionUpdated {
		t.Errorf("action = %q, want %q", action, ActionUpdated)
	}
	if _, ok := f.files["/etc/caddy/pilot.d/api.caddy"]; ok {
		t.Error("the old site block is still installed")
	}
	if strings.Count(strings.Join(f.cmds, "\n"), "caddy reload") != 1 {
		t.Errorf("want exactly one reload, ran:\n%s", strings.Join(f.cmds, "\n"))
	}
	if rm := slices.IndexFunc(f.cmds, func(c string) bool { return strings.HasPrefix(c, "rm -f") }); rm < 0 || rm > slices.IndexFunc(f.cmds, func(c string) bool { return strings.HasPrefix(c, "write ") }) {
		t.Errorf("the old block must go before the new files land, ran:\n%s", strings.Join(f.cmds, "\n"))
	}
}

func TestInstallSharedRouteRestoresEverythingOnFailure(t *testing.T) {
	files := map[string]string{
		testPaths.Caddyfile:            "import pilot.d/*.caddy\n",
		"/etc/caddy/pilot.d/api.caddy": legacyAPI,
	}
	f := newFake(files)
	f.ambiguous = "example.com"

	_, err := InstallSnippet(context.Background(), f, testPaths, "api", sharedAPI)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"ambiguous site definition", "_site.<domain>.caddy", "previous route was restored"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if len(f.files) != len(files) || f.files["/etc/caddy/pilot.d/api.caddy"] != legacyAPI {
		t.Errorf("host not back to where it was: %v", f.files)
	}
}

// Leaving the shared layout takes the route's handles with it, and the site
// block of a domain nobody else is routed into, but keeps one that still
// serves another service.
func TestInstallSiteRouteLeavesSharedSites(t *testing.T) {
	f := newFake(map[string]string{testPaths.Caddyfile: "import pilot.d/*.caddy\n"})
	ctx := context.Background()
	if _, err := InstallSnippet(ctx, f, testPaths, "api", sharedAPI); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallSnippet(ctx, f, testPaths, "site", sharedSite); err != nil {
		t.Fatal(err)
	}

	const own = "www.example.com {\n\trespond api\n}\n"
	f.cmds = nil
	if _, err := InstallSnippet(ctx, f, testPaths, "api", own); err != nil {
		t.Fatal(err)
	}
	if f.files["/etc/caddy/pilot.d/api.caddy"] != own {
		t.Error("the route's own block is missing")
	}
	for _, gone := range []string{"_site.example.com/api.caddy", "_site.www.example.com/api.caddy", "_site.www.example.com.caddy"} {
		if _, ok := f.files["/etc/caddy/pilot.d/"+gone]; ok {
			t.Errorf("%s should have gone", gone)
		}
	}
	if _, ok := f.files["/etc/caddy/pilot.d/_site.example.com.caddy"]; !ok {
		t.Error("the site block still serving site was removed")
	}
	if !f.ran("rmdir /etc/caddy/pilot.d/_site.www.example.com") {
		t.Errorf("the emptied handle directory was left, ran:\n%s", strings.Join(f.cmds, "\n"))
	}
}

func TestInstallSharedRouteChecksTheBind(t *testing.T) {
	f := newFake(map[string]string{testPaths.Caddyfile: "box.example.com {\n\tbind 10.0.0.1\n}\nimport pilot.d/*.caddy\n"})
	if _, err := InstallSnippet(context.Background(), f, testPaths, "site", sharedSite); err == nil || !strings.Contains(err.Error(), "caddy.bind") {
		t.Fatalf("want a refusal naming the fix, got: %v", err)
	}

	bound := strings.Replace(sharedSite, "handle", "# shared site bind: 10.0.0.1\nhandle", 1)
	if _, err := InstallSnippet(context.Background(), f, testPaths, "site", bound); err != nil {
		t.Fatalf("a bound shared route should install: %v", err)
	}
	if got := f.files["/etc/caddy/pilot.d/_site.example.com.caddy"]; !strings.Contains(got, "\tbind 10.0.0.1\n") {
		t.Errorf("the site block lacks the bind:\n%s", got)
	}
}

func TestRemoveSharedRoute(t *testing.T) {
	f := newFake(map[string]string{testPaths.Caddyfile: "import pilot.d/*.caddy\n"})
	ctx := context.Background()
	for svc, route := range map[string]string{"api": sharedAPI, "site": sharedSite} {
		if _, err := InstallSnippet(ctx, f, testPaths, svc, route); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ListSnippets(ctx, f, testPaths)
	if err != nil || strings.Join(got, ",") != "api,site" {
		t.Errorf("ListSnippets = %v, %v; want the services, not the site blocks", got, err)
	}

	action, err := RemoveSnippet(ctx, f, testPaths, "api")
	if err != nil || action != ActionRemoved {
		t.Fatalf("RemoveSnippet = %q, %v", action, err)
	}
	var left []string
	for p := range f.files {
		left = append(left, strings.TrimPrefix(p, "/etc/caddy/pilot.d/"))
	}
	slices.Sort(left)
	if want := "/etc/caddy/Caddyfile,_site.example.com.caddy,_site.example.com/site.caddy"; strings.Join(left, ",") != want {
		t.Errorf("left %v, want %s", left, want)
	}
}

func TestInstalled(t *testing.T) {
	read := func(files map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}
	}
	site := renderSite("example.com", nil)
	www := renderSite("www.example.com", nil)
	full := map[string]string{
		"/etc/caddy/pilot.d/_site.example.com/api.caddy":     sharedAPI,
		"/etc/caddy/pilot.d/_site.www.example.com/api.caddy": sharedAPI,
		"/etc/caddy/pilot.d/_site.example.com.caddy":         site,
		"/etc/caddy/pilot.d/_site.www.example.com.caddy":     www,
	}
	without := func(drop string, add map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range full {
			if k != drop {
				out[k] = v
			}
		}
		for k, v := range add {
			out[k] = v
		}
		return out
	}

	tests := []struct {
		name    string
		files   map[string]string
		want    string
		state   RouteState
		gotText string
	}{
		{"site block in place", map[string]string{"/etc/caddy/pilot.d/api.caddy": legacyAPI}, legacyAPI, RouteMatches, legacyAPI},
		{"site block absent", nil, legacyAPI, RouteMissing, ""},
		{"site block edited", map[string]string{"/etc/caddy/pilot.d/api.caddy": "x"}, legacyAPI, RouteDiffers, "x"},
		{"shared in place", full, sharedAPI, RouteMatches, sharedAPI},
		{"shared absent, other services' site blocks present", map[string]string{"/etc/caddy/pilot.d/_site.example.com.caddy": site}, sharedAPI, RouteMissing, ""},
		{"shared site block missing", without("/etc/caddy/pilot.d/_site.www.example.com.caddy", nil), sharedAPI, RouteDiffers, sharedAPI},
		{"shared site block binds elsewhere", without("", map[string]string{"/etc/caddy/pilot.d/_site.example.com.caddy": renderSite("example.com", []string{"10.0.0.1"})}), sharedAPI, RouteDiffers, sharedAPI},
		{"old site block still beside the handles", without("", map[string]string{"/etc/caddy/pilot.d/api.caddy": legacyAPI}), sharedAPI, RouteDiffers, sharedAPI},
		{"only the old site block", map[string]string{"/etc/caddy/pilot.d/api.caddy": legacyAPI}, sharedAPI, RouteDiffers, legacyAPI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, state := Installed(read(tt.files), testPaths, "api", tt.want)
			if state != tt.state || got != tt.gotText {
				t.Errorf("Installed = %q, %d; want %q, %d", got, state, tt.gotText, tt.state)
			}
		})
	}
}

func TestInstalledBinds(t *testing.T) {
	read := func(files map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := files[strings.TrimPrefix(p, "/etc/caddy/pilot.d/")]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}
	}
	bound := renderSite("example.com", []string{"10.0.0.1"})
	tests := []struct {
		name             string
		files            map[string]string
		installed, binds bool
	}{
		{"nothing installed", nil, false, false},
		{"own block with bind", map[string]string{"api.caddy": "example.com {\n\tbind 10.0.0.1\n}\n"}, true, true},
		{"own block without", map[string]string{"api.caddy": legacyAPI}, true, false},
		{"handles under a bound site", map[string]string{"_site.example.com/api.caddy": sharedAPI, "_site.example.com.caddy": bound}, true, true},
		{"handles under an unbound site", map[string]string{"_site.example.com/api.caddy": sharedAPI, "_site.example.com.caddy": renderSite("example.com", nil)}, true, false},
		{"handles with no site block", map[string]string{"_site.example.com/api.caddy": sharedAPI}, true, false},
		{"another service's site only", map[string]string{"_site.example.com.caddy": bound}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installed, binds := InstalledBinds(read(tt.files), testPaths, "api", []string{"example.com", "www.example.com"})
			if installed != tt.installed || binds != tt.binds {
				t.Errorf("InstalledBinds = %v, %v; want %v, %v", installed, binds, tt.installed, tt.binds)
			}
		})
	}
}

// A whole-domain block another service still owns would make Caddy reject
// the shared site block for the same domain. Writing first and undoing after
// validation would leave that rejected config on disk for a moment, and a
// Caddy restart then takes every site down, so the install refuses before it
// touches anything.
func TestInstallRefusesASecondBlockBeforeWriting(t *testing.T) {
	const siteBlock = "example.com {\n\trespond site\n}\n"
	t.Run("shared site beside another service's block", func(t *testing.T) {
		files := map[string]string{
			testPaths.Caddyfile:             "import pilot.d/*.caddy\n",
			"/etc/caddy/pilot.d/site.caddy": siteBlock,
			"/etc/caddy/pilot.d/api.caddy":  legacyAPI,
		}
		f := newFake(files)
		_, err := InstallSnippet(context.Background(), f, testPaths, "api", sharedAPI)
		if err == nil || !strings.Contains(err.Error(), "first deploy site") {
			t.Fatalf("want a refusal naming the service to deploy first, got: %v", err)
		}
		if f.ran("write ") || f.ran("rm -f") || f.ran("caddy validate") || f.ran("caddy reload") {
			t.Errorf("refused, yet touched the host:\n%s", strings.Join(f.cmds, "\n"))
		}
		if len(f.files) != len(files) {
			t.Errorf("files changed: %v", f.files)
		}
	})

	t.Run("the status page's block names its own remedy", func(t *testing.T) {
		f := newFake(map[string]string{
			testPaths.Caddyfile:                "import pilot.d/*.caddy\n",
			"/etc/caddy/pilot.d/_status.caddy": siteBlock,
		})
		_, err := InstallSnippet(context.Background(), f, testPaths, "api", sharedAPI)
		if err == nil || !strings.Contains(err.Error(), "pilot doctor --fix") || strings.Contains(err.Error(), "deploy _status") {
			t.Fatalf("want the status route's remedy, got: %v", err)
		}
	})

	t.Run("a failed search refuses rather than passes", func(t *testing.T) {
		f := newFake(map[string]string{testPaths.Caddyfile: "import pilot.d/*.caddy\n"})
		f.grepFails = true
		_, err := InstallSnippet(context.Background(), f, testPaths, "api", sharedAPI)
		if err == nil || !strings.Contains(err.Error(), "Permission denied") {
			t.Fatalf("want the search error, got: %v", err)
		}
		if f.ran("write ") {
			t.Error("installed without knowing whether the domain is declared twice")
		}
	})

	t.Run("the service's own old block moves in the same install", func(t *testing.T) {
		f := newFake(map[string]string{
			testPaths.Caddyfile:             "import pilot.d/*.caddy\n",
			"/etc/caddy/pilot.d/site.caddy": siteBlock,
		})
		if _, err := InstallSnippet(context.Background(), f, testPaths, "site", sharedSite); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.files["/etc/caddy/pilot.d/site.caddy"]; ok {
			t.Error("the old block is still installed")
		}
	})

	t.Run("an existing shared site needs no search", func(t *testing.T) {
		f := newFake(map[string]string{testPaths.Caddyfile: "import pilot.d/*.caddy\n"})
		if _, err := InstallSnippet(context.Background(), f, testPaths, "site", sharedSite); err != nil {
			t.Fatal(err)
		}
		f.cmds = nil
		if _, err := InstallSnippet(context.Background(), f, testPaths, "api", sharedAPI); err != nil {
			t.Fatal(err)
		}
		if f.ran("grep -lxF -e 'example.com {'") {
			t.Error("searched for a block beside a shared site that already validated")
		}
	})

	t.Run("own block beside other services' handles", func(t *testing.T) {
		f := newFake(map[string]string{testPaths.Caddyfile: "import pilot.d/*.caddy\n"})
		if _, err := InstallSnippet(context.Background(), f, testPaths, "api", sharedAPI); err != nil {
			t.Fatal(err)
		}
		before := maps.Clone(f.files)
		f.cmds = nil
		_, err := InstallSnippet(context.Background(), f, testPaths, "site", siteBlock)
		if err == nil || !strings.Contains(err.Error(), "pilot routes --prune") || !strings.Contains(err.Error(), "routes of api") {
			t.Fatalf("want a refusal naming api and the prune, got: %v", err)
		}
		if f.ran("write ") || !maps.Equal(before, f.files) {
			t.Error("refused, yet touched the host")
		}
	})
}

// A site block left with no handles, by a failed change or a hand edit, must
// not wedge the route that wants the domain to itself: beside it, Caddy would
// reject that route's block on every deploy.
func TestInstallRemovesAnOrphanSiteBlock(t *testing.T) {
	const own = "example.com {\n\trespond site\n}\n"
	f := newFake(map[string]string{
		testPaths.Caddyfile:                          "import pilot.d/*.caddy\n",
		"/etc/caddy/pilot.d/_site.example.com.caddy": renderSite("example.com", nil),
		"/etc/caddy/pilot.d/_site.other.com.caddy":   renderSite("other.com", nil),
	})
	if _, err := InstallSnippet(context.Background(), f, testPaths, "site", own); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files["/etc/caddy/pilot.d/_site.example.com.caddy"]; ok {
		t.Error("the orphan site block of the route's domain is still installed")
	}
	if _, ok := f.files["/etc/caddy/pilot.d/_site.other.com.caddy"]; !ok {
		t.Error("a site block of another domain is not this route's to remove")
	}
}

func TestTidySites(t *testing.T) {
	bound := strings.Replace(sharedAPI, "handle", "# shared site bind: 10.0.0.1\nhandle", 1)
	f := newFake(map[string]string{
		testPaths.Caddyfile:                                  "import pilot.d/*.caddy\n",
		"/etc/caddy/pilot.d/_site.empty.com.caddy":           renderSite("empty.com", nil),
		"/etc/caddy/pilot.d/_site.example.com/api.caddy":     bound,
		"/etc/caddy/pilot.d/_site.www.example.com.caddy":     renderSite("www.example.com", []string{"10.0.0.1"}),
		"/etc/caddy/pilot.d/_site.www.example.com/api.caddy": bound,
	})
	ctx := context.Background()

	empty, headless, err := SiteProblems(ctx, f, testPaths)
	if err != nil || strings.Join(empty, ",") != "_site.empty.com.caddy" || len(headless) != 1 || headless[0].Dir != "_site.example.com" || strings.Join(headless[0].Services, ",") != "api" {
		t.Fatalf("SiteProblems = %v, %v, %v", empty, headless, err)
	}

	changed, err := TidySites(ctx, f, testPaths)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(changed, ",") != "_site.empty.com.caddy,_site.example.com.caddy" {
		t.Errorf("changed %v", changed)
	}
	if _, ok := f.files["/etc/caddy/pilot.d/_site.empty.com.caddy"]; ok {
		t.Error("the empty site block is still there")
	}
	if got := f.files["/etc/caddy/pilot.d/_site.example.com.caddy"]; got != renderSite("example.com", []string{"10.0.0.1"}) {
		t.Errorf("rebuilt site block:\n%s", got)
	}
	if empty, headless, _ := SiteProblems(ctx, f, testPaths); len(empty)+len(headless) != 0 {
		t.Errorf("still %v, %v", empty, headless)
	}

	f.files["/etc/caddy/pilot.d/_site.lost.com/x.caddy"] = "lost.com {\n}\n"
	if _, err := TidySites(ctx, f, testPaths); err == nil || !strings.Contains(err.Error(), "redeploy") {
		t.Errorf("a handle directory with nothing naming its domain: %v", err)
	}
}

func TestInstallRefusesWhileAnotherPilotHoldsTheLock(t *testing.T) {
	f := newFake(map[string]string{testPaths.Caddyfile: "import pilot.d/*.caddy\n"})
	f.lockHeld = true
	ctx := context.Background()

	if _, err := InstallSnippet(ctx, f, testPaths, "api", sharedAPI); err == nil || !strings.Contains(err.Error(), ".pilot-lock") {
		t.Fatalf("want the held lock named, got: %v", err)
	}
	if _, err := RemoveSnippet(ctx, f, testPaths, "api"); err == nil {
		t.Error("RemoveSnippet ran under another Pilot's lock")
	}
	if _, err := TidySites(ctx, f, testPaths); err == nil {
		t.Error("TidySites ran under another Pilot's lock")
	}
	if f.ran("write ") || f.ran("caddy reload") {
		t.Error("changed routes without the lock")
	}
}

// The lock is a shell script run on the host, so it is tested in a real
// shell, by runners standing for Pilots on one host.
func TestRouteLockOnAHost(t *testing.T) {
	p := Paths{SnippetDir: filepath.Join(t.TempDir(), "pilot.d"), LockWait: time.Second}
	lock := filepath.Join(p.SnippetDir, lockDir)
	ctx := context.Background()
	agent, cli := local.New("web-1"), local.New("web-1")
	age := func() {
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
	}

	unlock, err := lockRoutes(ctx, agent, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockRoutes(ctx, cli, p); err == nil || !strings.Contains(err.Error(), "still held after waiting 1s") {
		t.Fatalf("second holder: %v", err)
	}
	unlock()
	unlockCLI, err := lockRoutes(ctx, cli, p)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}

	// A holder that outlived LockStale finds its lock broken. Its late
	// release must leave the new holder's lock alone.
	age()
	unlockAgent, err := lockRoutes(ctx, agent, p)
	if err != nil {
		t.Fatalf("a stale lock was not broken: %v", err)
	}
	unlockCLI()
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a late release removed the new holder's lock: %v", err)
	}
	unlockAgent()
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

// Two waiters that find the same stale lock must not both break it and both
// hold the lock.
func TestRouteLockStaleBreakIsExclusive(t *testing.T) {
	for range 3 {
		p := Paths{SnippetDir: filepath.Join(t.TempDir(), "pilot.d"), LockWait: 2 * time.Second}
		lock := filepath.Join(p.SnippetDir, lockDir)
		if err := os.MkdirAll(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lock, "owner"), []byte("dead\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}

		type result struct {
			unlock func()
			err    error
		}
		results := make(chan result, 2)
		for range 2 {
			go func() {
				unlock, err := lockRoutes(context.Background(), local.New("web-1"), p)
				results <- result{unlock, err}
			}()
		}
		var held []func()
		var refused int
		for range 2 {
			r := <-results
			if r.err != nil {
				refused++
				continue
			}
			held = append(held, r.unlock)
		}
		for _, u := range held {
			u()
		}
		if len(held) != 1 || refused != 1 {
			t.Fatalf("%d holders and %d refused, want one of each", len(held), refused)
		}
	}
}

func TestSharedTexts(t *testing.T) {
	read := func(p string) ([]byte, error) {
		files := map[string]string{
			"/etc/caddy/pilot.d/_site.example.com/api.caddy":     sharedAPI,
			"/etc/caddy/pilot.d/_site.www.example.com/api.caddy": sharedAPI,
			"/etc/caddy/pilot.d/_site.example.com.caddy":         renderSite("example.com", nil),
		}
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}
	want, got := SharedTexts(read, testPaths, "api", sharedAPI)
	if want == got || !strings.Contains(got, "# _site.www.example.com.caddy (missing)\n") {
		t.Errorf("the missing site block is not shown:\n%s", got)
	}
	if !strings.Contains(want, "# _site.www.example.com.caddy\n"+renderSite("www.example.com", nil)) {
		t.Errorf("want lacks the site block:\n%s", want)
	}
}

// The listing decides which old files a change removes and which site blocks
// go, so an unreadable snippet dir must stop the change rather than read as
// a host with no routes.
func TestRouteChangesRefuseAnUnreadableListing(t *testing.T) {
	f := newFake(map[string]string{
		testPaths.Caddyfile:            "import pilot.d/*.caddy\n",
		"/etc/caddy/pilot.d/api.caddy": legacyAPI,
	})
	f.listFails = true
	ctx := context.Background()

	if _, err := InstallSnippet(ctx, f, testPaths, "api", sharedAPI); err == nil || !strings.Contains(err.Error(), "listing the routes") {
		t.Errorf("InstallSnippet: %v", err)
	}
	if _, err := RemoveSnippet(ctx, f, testPaths, "api"); err == nil {
		t.Error("RemoveSnippet read an unreadable listing as empty")
	}
	if _, err := ListSnippets(ctx, f, testPaths); err == nil {
		t.Error("ListSnippets read an unreadable listing as empty")
	}
	if f.ran("write ") || f.ran("rm -f") {
		t.Error("changed routes without a listing")
	}
}
