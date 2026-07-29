package caddy

import (
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

func TestRenderProxy(t *testing.T) {
	got, err := Render(Input{
		Service: "api",
		Expose: &config.Expose{
			Domains:  []string{"api.example.com"},
			Upstream: 8080,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `# managed by pilot — service: api — do not edit
# edits are reverted on the next deploy; change services/api.yaml instead
api.example.com {
	encode gzip zstd
	reverse_proxy 127.0.0.1:8080 {
		lb_try_duration 10s
		lb_try_interval 250ms
	}
}
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderProxyWithPathAndTimeouts(t *testing.T) {
	got, err := Render(Input{
		Service: "api",
		Expose: &config.Expose{
			Domains:  []string{"a.example.com", "b.example.com"},
			Path:     "/v1/*",
			Upstream: 9000,
			Timeouts: &config.Timeouts{
				Read: config.Duration(60_000_000_000),
				Dial: config.Duration(5_000_000_000),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"a.example.com/v1, b.example.com/v1 {",
		"\t\tdial_timeout 5s\n",
		"\t\tread_timeout 1m0s\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "write_timeout") {
		t.Error("unset timeout should be omitted entirely")
	}
}

func TestRenderStaticSPA(t *testing.T) {
	got, err := Render(Input{
		Service: "blog",
		Root:    "/opt/pilot/services/blog/current",
		Expose: &config.Expose{
			Domains: []string{"blog.example.com"},
			Static: &config.StaticExpose{
				SPA:   true,
				Index: "index.html",
				Headers: map[string]map[string]string{
					"/assets/*": {"Cache-Control": "public, max-age=31536000, immutable"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `# managed by pilot — service: blog — do not edit
# edits are reverted on the next deploy; change services/blog.yaml instead
blog.example.com {
	encode gzip zstd
	root * /opt/pilot/services/blog/current
	header /assets/* Cache-Control "public, max-age=31536000, immutable"
	try_files {path} {path}/ /index.html
	file_server
}
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Map iteration order must not leak into the file, or every deploy would look
// like a routing change and force a needless Caddy reload.
func TestRenderIsDeterministic(t *testing.T) {
	in := Input{
		Service: "blog",
		Root:    "/srv/blog/current",
		Expose: &config.Expose{
			Domains: []string{"blog.example.com"},
			Static: &config.StaticExpose{
				Headers: map[string]map[string]string{
					"/z/*":      {"X-Two": "2", "X-One": "1"},
					"/a/*":      {"X-Three": "3"},
					"/assets/*": {"Cache-Control": "immutable"},
				},
			},
		},
	}
	first, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		got, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("render is not deterministic:\n%s\nvs\n%s", first, got)
		}
	}
	// Matchers sorted, then header names within each matcher.
	wantOrder := []string{"/a/* X-Three", "/assets/* Cache-Control", "/z/* X-One", "/z/* X-Two"}
	last := -1
	for _, w := range wantOrder {
		idx := strings.Index(first, w)
		if idx < 0 {
			t.Fatalf("missing %q in:\n%s", w, first)
		}
		if idx < last {
			t.Errorf("%q is out of order in:\n%s", w, first)
		}
		last = idx
	}
}

func TestRenderRawEscapeHatch(t *testing.T) {
	got, err := Render(Input{
		Service: "api",
		Expose: &config.Expose{
			Domains:  []string{"api.example.com"},
			Upstream: 8080,
			Raw:      "header /admin/* X-Robots-Tag noindex\nbasicauth /admin/* {\n  bob hash\n}",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "\theader /admin/* X-Robots-Tag noindex\n") {
		t.Errorf("raw lines should be indented into the site block:\n%s", got)
	}
	if !strings.HasSuffix(got, "}\n") {
		t.Errorf("site block should still close cleanly:\n%s", got)
	}
}

func TestRenderRejectsIncompleteInput(t *testing.T) {
	tests := []struct {
		name string
		in   Input
	}{
		{"no expose", Input{Service: "x"}},
		{"no domains", Input{Service: "x", Expose: &config.Expose{Upstream: 80}}},
		{"static without root", Input{Service: "x", Expose: &config.Expose{
			Domains: []string{"a.com"}, Static: &config.StaticExpose{}}}},
		{"proxy without port", Input{Service: "x", Expose: &config.Expose{
			Domains: []string{"a.com"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Render(tc.in); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestImportDirective(t *testing.T) {
	tests := []struct {
		caddyfile, snippetDir, want string
	}{
		{"/etc/caddy/Caddyfile", "/etc/caddy/pilot.d", "import pilot.d/*.caddy"},
		{"/etc/caddy/Caddyfile", "/etc/caddy/sub/dir", "import sub/dir/*.caddy"},
		{"/etc/caddy/Caddyfile", "/srv/pilot.d", "import /srv/pilot.d/*.caddy"},
	}
	for _, tc := range tests {
		if got := ImportDirective(tc.caddyfile, tc.snippetDir); got != tc.want {
			t.Errorf("ImportDirective(%q, %q) = %q, want %q", tc.caddyfile, tc.snippetDir, got, tc.want)
		}
	}
}

func TestInspect(t *testing.T) {
	const cf, sd = "/etc/caddy/Caddyfile", "/etc/caddy/pilot.d"

	tests := []struct {
		name    string
		content string
		want    ImportState
	}{
		{"empty file", "", ImportMissing},
		{"comments only", "# nothing here\n\n# really\n", ImportMissing},
		{
			"braced site",
			"example.com {\n\troot * /var/www\n\tfile_server\n}\n",
			ImportMissing,
		},
		{
			"global options block",
			"{\n\temail me@example.com\n}\n\nexample.com {\n\tfile_server\n}\n",
			ImportMissing,
		},
		{
			"already imported",
			"example.com {\n\tfile_server\n}\n\nimport pilot.d/*.caddy\n",
			ImportPresent,
		},
		{
			"imported by absolute path",
			"import /etc/caddy/pilot.d/*.caddy\n",
			ImportPresent,
		},
		{
			"imported with quotes",
			"import \"pilot.d/*.caddy\"\n",
			ImportPresent,
		},
		{
			"unrelated import is not ours",
			"import sites/*.caddy\n",
			ImportMissing,
		},
		{
			"brace-less single site",
			"example.com\nroot * /var/www\nfile_server\n",
			ImportUnsafe,
		},
		{
			"brace-less with a placeholder directive",
			"example.com\ntry_files {path} /index.html\nfile_server\n",
			ImportUnsafe,
		},
		{
			"placeholder inside a braced site is fine",
			"example.com {\n\ttry_files {path} /index.html\n\tfile_server\n}\n",
			ImportMissing,
		},
		{
			"braces in a quoted value do not confuse depth",
			"example.com {\n\trespond \"{not a block\"\n}\n",
			ImportMissing,
		},
		{
			"snippet definition",
			"(common) {\n\tencode gzip\n}\n\nexample.com {\n\timport common\n}\n",
			ImportMissing,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Inspect(tc.content, cf, sd)
			if got != tc.want {
				t.Errorf("Inspect = %v (%s), want %v\ncontent:\n%s", got, reason, tc.want, tc.content)
			}
			if got == ImportUnsafe && reason == "" {
				t.Error("an unsafe verdict must explain itself")
			}
		})
	}
}

func TestAppendImport(t *testing.T) {
	const cf, sd = "/etc/caddy/Caddyfile", "/etc/caddy/pilot.d"

	t.Run("appends once", func(t *testing.T) {
		orig := "example.com {\n\tfile_server\n}\n"
		got, err := AppendImport(orig, cf, sd)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got, orig) {
			t.Error("existing content must be preserved verbatim")
		}
		if !strings.Contains(got, "import pilot.d/*.caddy") {
			t.Errorf("directive not added:\n%s", got)
		}
		if state, _ := Inspect(got, cf, sd); state != ImportPresent {
			t.Errorf("result should inspect as present, got %v", state)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		once, err := AppendImport("example.com {\n\tfile_server\n}\n", cf, sd)
		if err != nil {
			t.Fatal(err)
		}
		twice, err := AppendImport(once, cf, sd)
		if err != nil {
			t.Fatal(err)
		}
		if once != twice {
			t.Errorf("second append changed the file:\n%s", twice)
		}
	})

	t.Run("handles missing trailing newline", func(t *testing.T) {
		got, err := AppendImport("example.com {\n\tfile_server\n}", cf, sd)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "}import") {
			t.Errorf("newline not inserted:\n%s", got)
		}
	})

	t.Run("refuses the brace-less form", func(t *testing.T) {
		_, err := AppendImport("example.com\nfile_server\n", cf, sd)
		if err == nil {
			t.Fatal("want a refusal")
		}
		if !strings.Contains(err.Error(), "wrap the existing site in braces") {
			t.Errorf("error should carry the remedy, got: %v", err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		got, err := AppendImport("", cf, sd)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(got, "\n") {
			t.Errorf("no leading blank line for an empty file:\n%q", got)
		}
	})
}

// A raw block is where provider-specific config lands — a DNS-01 TLS stanza,
// for instance. Flattening its nesting would still parse, but produces a
// generated file nobody can read.
func TestRenderRawPreservesNesting(t *testing.T) {
	got, err := Render(Input{
		Service: "kite",
		Expose: &config.Expose{
			Domains:  []string{"kite.example.com"},
			Upstream: 8085,
			Raw: `tls {
    dns ovh {
        endpoint ovh-eu
        application_key {env.OVH_APPLICATION_KEY}
    }
}`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"\ttls {\n",
		"\t    dns ovh {\n",
		"\t        endpoint ovh-eu\n",
		"\t        application_key {env.OVH_APPLICATION_KEY}\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// Braces must still balance, or Caddy rejects the whole file.
	if strings.Count(got, "{") != strings.Count(got, "}") {
		t.Errorf("unbalanced braces:\n%s", got)
	}
}

// Common leading indentation in the YAML block scalar is an artefact of the
// config file, not something to reproduce in the output.
func TestRenderRawStripsCommonIndent(t *testing.T) {
	got, err := Render(Input{
		Service: "x",
		Expose: &config.Expose{
			Domains: []string{"x.example.com"}, Upstream: 80,
			Raw: "        header X-A 1\n        header X-B 2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "\theader X-A 1\n") {
		t.Errorf("common indent not stripped:\n%s", got)
	}
}

// A restricted route must not leave an unconditional path to the service
// alongside the guarded one — that was the bug in the hand-written config this
// replaces, where a 0.0.0.0 port bypassed the rule entirely.
func TestRenderRestrictedRoute(t *testing.T) {
	got, err := Render(Input{
		Service: "paperless",
		Expose: &config.Expose{
			Domains:  []string{"paperless.example.com"},
			Upstream: 8000,
			Allow:    config.TailnetCIDRs,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"@allowed remote_ip 100.64.0.0/10 fd7a:115c:a1e0::/48",
		"handle @allowed {",
		"\t\treverse_proxy 127.0.0.1:8000 {",
		"handle {",
		"abort",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// Exactly one proxy, and it is inside the guard.
	if n := strings.Count(got, "reverse_proxy"); n != 1 {
		t.Errorf("%d reverse_proxy directives; a second one would serve everyone:\n%s", n, got)
	}
	if strings.Index(got, "handle @allowed") > strings.Index(got, "reverse_proxy") {
		t.Errorf("the proxy must be inside the guard:\n%s", got)
	}
	if strings.Count(got, "{") != strings.Count(got, "}") {
		t.Errorf("unbalanced braces:\n%s", got)
	}
}

func TestRenderRestrictedStaticRoute(t *testing.T) {
	got, err := Render(Input{
		Service: "docs", Root: "/opt/pilot/services/docs/current",
		Expose: &config.Expose{
			Domains: []string{"docs.example.com"},
			Static:  &config.StaticExpose{},
			Allow:   []string{"10.0.0.0/8"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "@allowed remote_ip 10.0.0.0/8") || !strings.Contains(got, "abort") {
		t.Errorf("static routes should be restrictable too:\n%s", got)
	}
	if n := strings.Count(got, "file_server"); n != 1 {
		t.Errorf("%d file_server directives:\n%s", n, got)
	}
}

func TestUnrestrictedRouteHasNoGuard(t *testing.T) {
	got, err := Render(Input{
		Service: "api",
		Expose:  &config.Expose{Domains: []string{"api.example.com"}, Upstream: 8080},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "handle") || strings.Contains(got, "abort") {
		t.Errorf("an unrestricted route should stay simple:\n%s", got)
	}
}
