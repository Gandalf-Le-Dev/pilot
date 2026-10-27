// Package caddy renders Pilot's routing config into Caddyfile snippets and
// manages the single import line Pilot adds to the global Caddyfile.
//
// Pilot owns exactly one file per exposed service, under the configured snippet
// directory. Everything else in Caddy's config belongs to the operator.
package caddy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

// Retry behaviour applied to every proxied route. During a container recreate
// Caddy holds the request and retries rather than returning 502, which turns a
// few seconds of downtime into a few seconds of latency.
const (
	TryDuration = "10s"
	TryInterval = "250ms"
)

// Input is everything needed to render one service's route.
//
// Note there is deliberately no release identifier here. The snippet must be
// byte-identical between deploys that don't change routing, so that Pilot can
// compare hashes and skip the Caddy reload entirely. Release provenance belongs
// in the release manifest, not in a file whose stability we depend on.
type Input struct {
	Service string
	Expose  *config.Expose

	// Root is the absolute directory Caddy serves for a static route. It should
	// point at the service's `current` symlink so that a release swap is picked
	// up without touching Caddy at all.
	Root string

	// Bind lists the addresses the site block binds to, from the host's
	// caddy.bind. Caddy groups sites into servers by listen address, so on a
	// host whose own Caddyfile binds sites explicitly, a generated block
	// without the same bind would land in a server public traffic never
	// reaches. Empty means no bind line, which is right for hosts that don't
	// bind explicitly.
	Bind []string

	// Shared renders the route as handles inside Pilot's site block for each
	// domain, because another service on one of its hosts serves a path on
	// one of its domains. A route with a path is always shared.
	Shared bool
}

// Render produces a service's route: a site block per domain, or for a shared
// route, the handles installed under each domain's shared site block.
func Render(in Input) (string, error) {
	e := in.Expose
	if e == nil {
		return "", fmt.Errorf("service %q has no expose block", in.Service)
	}
	if len(e.Domains) == 0 {
		return "", fmt.Errorf("service %q exposes no domains", in.Service)
	}
	if e.IsStatic() && in.Root == "" {
		return "", fmt.Errorf("service %q is a static route with no root", in.Service)
	}
	if !e.IsStatic() && e.Upstream == 0 {
		return "", fmt.Errorf("service %q is a proxy route with no upstream port", in.Service)
	}

	prefix := e.PathPrefix()
	shared := in.Shared || prefix != ""
	if shared {
		if d := e.SiteDirective(); d != "" {
			return "", fmt.Errorf("service %q shares its domains with other routes, so its expose.raw cannot use `%s`: "+
				"that configures the whole site, which Pilot's shared site block serves for every service on the domain", in.Service, d)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# managed by pilot — service: %s — do not edit\n", in.Service)
	fmt.Fprintf(&b, "# edits are reverted on the next deploy; change services/%s.yaml instead\n", in.Service)
	if shared {
		fmt.Fprintf(&b, "%s%s\n", sharedMarker, strings.Join(e.Domains, " "))
		if len(in.Bind) > 0 {
			fmt.Fprintf(&b, "%s%s\n", sharedBindMarker, strings.Join(in.Bind, " "))
		}
	}

	var body strings.Builder
	if len(in.Bind) > 0 && !shared {
		fmt.Fprintf(&body, "\tbind %s\n", strings.Join(in.Bind, " "))
	}
	body.WriteString("\tencode gzip zstd\n")

	// A restricted route wraps its body so anything outside the allowed
	// networks is closed on rather than served. `abort` rather than `respond
	// 403` is deliberate: a closed connection tells a scanner nothing.
	if e.Restricted() {
		fmt.Fprintf(&body, "\t@allowed remote_ip %s\n", strings.Join(e.Allow, " "))
		body.WriteString("\thandle @allowed {\n")
		var inner strings.Builder
		if e.IsStatic() {
			renderStatic(&inner, e, in.Root)
		} else {
			renderProxy(&inner, e)
		}
		for line := range strings.SplitSeq(strings.TrimRight(inner.String(), "\n"), "\n") {
			fmt.Fprintf(&body, "\t%s\n", line)
		}
		body.WriteString("\t}\n\thandle {\n\t\tabort\n\t}\n")
	} else if e.IsStatic() {
		renderStatic(&body, e, in.Root)
	} else {
		renderProxy(&body, e)
	}

	if raw := strings.TrimRight(e.Raw, "\n"); raw != "" {
		body.WriteString("\n")
		writeRaw(&body, raw)
	}

	if shared {
		// A path matcher matches exactly and `/v1/*` does not match bare
		// `/v1`, so a prefix takes both; `/v1*` would also catch `/v1beta`.
		// They are two handles rather than one with both paths because Caddy
		// sorts handles deepest first only when each has a single path, and
		// that order is what lets another service's /v1/admin beat /v1.
		matchers := []string{""}
		if prefix != "" {
			matchers = []string{prefix + " ", prefix + "/* "}
		}
		for i, m := range matchers {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "handle %s{\n%s}\n", m, body.String())
		}
		return b.String(), nil
	}

	// One block per domain, the form these routes always had: a domain nobody
	// shares keeps a byte-identical route, and its next deploy needs no reload.
	for i, d := range e.Domains {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s {\n%s}\n", d, body.String())
	}
	return b.String(), nil
}

func renderProxy(b *strings.Builder, e *config.Expose) {
	fmt.Fprintf(b, "\treverse_proxy 127.0.0.1:%d {\n", e.Upstream)
	fmt.Fprintf(b, "\t\tlb_try_duration %s\n", TryDuration)
	fmt.Fprintf(b, "\t\tlb_try_interval %s\n", TryInterval)

	if t := e.Timeouts; t != nil && (!t.Dial.IsZero() || !t.Read.IsZero() || !t.Write.IsZero()) {
		b.WriteString("\t\ttransport http {\n")
		if !t.Dial.IsZero() {
			fmt.Fprintf(b, "\t\t\tdial_timeout %s\n", t.Dial)
		}
		if !t.Read.IsZero() {
			fmt.Fprintf(b, "\t\t\tread_timeout %s\n", t.Read)
		}
		if !t.Write.IsZero() {
			fmt.Fprintf(b, "\t\t\twrite_timeout %s\n", t.Write)
		}
		b.WriteString("\t\t}\n")
	}

	b.WriteString("\t}\n")
}

func renderStatic(b *strings.Builder, e *config.Expose, root string) {
	fmt.Fprintf(b, "\troot * %s\n", root)

	// Sort both matchers and header names so output is stable across runs;
	// Go map iteration order would otherwise churn the file and force a
	// pointless Caddy reload on every deploy.
	s := e.Static
	for _, matcher := range sortedKeys(s.Headers) {
		for _, name := range sortedKeys(s.Headers[matcher]) {
			fmt.Fprintf(b, "\theader %s %s %q\n", matcher, name, s.Headers[matcher][name])
		}
	}

	if s.SPA {
		index := s.Index
		if index == "" {
			index = config.DefaultIndex
		}
		fmt.Fprintf(b, "\ttry_files {path} {path}/ /%s\n", strings.TrimPrefix(index, "/"))
	}

	b.WriteString("\tfile_server\n")
}

// writeRaw indents a raw block into the site body, preserving the relative
// indentation the author wrote.
//
// Flattening every line to one tab would still parse — Caddyfile blocks are
// delimited by braces, not indentation — but a nested `tls { dns ovh { … } }`
// would come out unreadable, and a generated file nobody can read is a file
// people work around rather than with.
func writeRaw(b *strings.Builder, raw string) {
	lines := strings.Split(raw, "\n")

	// The smallest indent among non-blank lines becomes the new baseline.
	base := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " \t")); base < 0 || n < base {
			base = n
		}
	}
	if base < 0 {
		base = 0
	}

	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			b.WriteString("\n")
			continue
		}
		trimmed := l
		if len(l) >= base {
			trimmed = l[base:]
		}
		fmt.Fprintf(b, "\t%s\n", strings.TrimRight(trimmed, " \t"))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SnippetName is the file Pilot owns for a service, relative to the snippet dir.
func SnippetName(service string) string { return service + ".caddy" }
