package main

import (
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

func TestSharedSitesFirst(t *testing.T) {
	expose := func(path string) *config.Expose {
		return &config.Expose{Domains: []string{"example.com"}, Path: path, Upstream: 8080}
	}
	api := &config.Service{Name: "api", Hosts: []string{"web-1"}, Expose: expose("/v1")}
	blog := &config.Service{Name: "blog", Hosts: []string{"web-1"}, Expose: &config.Expose{Domains: []string{"blog.example.com"}, Upstream: 8082}}
	site := &config.Service{Name: "site", Hosts: []string{"web-1"}, Expose: expose("")}
	worker := &config.Service{Name: "worker", Hosts: []string{"web-1"}}
	f := &config.Fleet{Services: map[string]*config.Service{"api": api, "blog": blog, "site": site, "worker": worker}}

	var got []string
	for _, s := range sharedSitesFirst(f, []*config.Service{api, blog, site, worker}) {
		got = append(got, s.Name)
	}
	if strings.Join(got, ",") != "site,api,blog,worker" {
		t.Errorf("order = %v, want site first and the rest as given", got)
	}
}
