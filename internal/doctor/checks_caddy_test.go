package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
)

func TestCaddyPathFindings(t *testing.T) {
	expose := func(path string) *config.Expose {
		return &config.Expose{Domains: []string{"example.com"}, Path: path, Upstream: 8080}
	}
	api := &config.Service{Name: "api", Expose: expose("/v1/*")}
	site := &config.Service{Name: "site", Expose: expose("")}
	services := []*config.Service{api, site}

	got := caddyPathFindings("web-1", services, map[string]string{
		"api":  "example.com/v1, example.com/v1/* {\n}\n",
		"site": "example.com {\n}\n",
	})
	if len(got) != 2 {
		t.Fatalf("findings = %+v, want one per route", got)
	}
	if !strings.Contains(got[0].Title, "puts its path in the site address") || !strings.HasPrefix(got[0].Hint, "redeploy site, then api ") {
		t.Errorf("the path route must wait for site: %+v", got[0])
	}
	if !strings.Contains(got[1].Title, "serves example.com as a site of its own") || !strings.HasPrefix(got[1].Hint, "redeploy site first") {
		t.Errorf("site's own block: %+v", got[1])
	}

	got = caddyPathFindings("web-1", services, map[string]string{"api": "example.com/v1 {\n}\n"})
	if len(got) != 1 || got[0].Status != StatusFail || !strings.HasPrefix(got[0].Hint, "redeploy api ") {
		t.Errorf("an exact path with site already moved: %+v", got)
	}
}

// doctor --fix runs fixes in finding order, so a removed service's routes go
// before a shared site block is rebuilt, and a handle directory holding only
// such routes gets no site block at all.
func TestCaddyRouteFindings(t *testing.T) {
	var ran []string
	remove := func(svc string) func(context.Context) error {
		return func(context.Context) error { ran = append(ran, "remove "+svc); return nil }
	}
	tidy := func(context.Context) error { ran = append(ran, "tidy"); return nil }

	got := caddyRouteFindings("web-1",
		[]string{"api", "gone", "old"},
		map[string]bool{"api": true},
		[]string{"_site.empty.com.caddy"},
		[]caddy.HeadlessSite{
			{Dir: "_site.example.com", Services: []string{"api", "gone"}},
			{Dir: "_site.removed.com", Services: []string{"old"}},
		},
		remove, tidy)

	var titles []string
	for _, f := range got {
		titles = append(titles, f.Title)
		if err := f.Fix(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"orphaned route: gone.caddy",
		"orphaned route: old.caddy",
		"shared site block with no routes in it: _site.empty.com.caddy",
		"routes with no site block: _site.example.com/",
	}
	if strings.Join(titles, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(titles, "\n"), strings.Join(want, "\n"))
	}
	if strings.Join(ran, ",") != "remove gone,remove old,tidy,tidy" {
		t.Errorf("fixes ran as %v, want the removals first", ran)
	}
}

func TestProxiesToUpstream(t *testing.T) {
	s := &config.Service{
		Name:    "api",
		Expose:  &config.Expose{Domains: []string{"api.example.com"}, Upstream: 8080},
		Rollout: &config.Rollout{Strategy: config.StrategyBlueGreen, Service: "web", Ports: []int{18080, 18081}},
	}
	for route, want := range map[string]bool{
		"api.example.com {\n\treverse_proxy 127.0.0.1:8080 {\n\t}\n}\n":  true,
		"api.example.com {\n\treverse_proxy 127.0.0.1:18081 {\n\t}\n}\n": false,
		"handle {\n\treverse_proxy 127.0.0.1:8080 {\n\t}\n}\n":           true,
	} {
		if got := proxiesToUpstream(s, route); got != want {
			t.Errorf("proxiesToUpstream(%q) = %v, want %v", route, got, want)
		}
	}
	if f := bluegreenRouteFinding("web-1", s); f.Status != StatusFail || !strings.HasPrefix(f.Hint, "redeploy api") {
		t.Errorf("finding = %+v", f)
	}
}
