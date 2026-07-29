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
}

// Render produces the complete contents of <snippet_dir>/<service>.caddy.
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

	var b strings.Builder
	fmt.Fprintf(&b, "# managed by pilot — service: %s — do not edit\n", in.Service)
	fmt.Fprintf(&b, "# edits are reverted on the next deploy; change services/%s.yaml instead\n", in.Service)

	fmt.Fprintf(&b, "%s {\n", siteAddress(e))
	b.WriteString("\tencode gzip zstd\n")

	// A restricted route wraps its body so anything outside the allowed
	// networks is closed on rather than served. `abort` rather than `respond
	// 403` is deliberate: a closed connection tells a scanner nothing.
	if e.Restricted() {
		fmt.Fprintf(&b, "\t@allowed remote_ip %s\n", strings.Join(e.Allow, " "))
		b.WriteString("\thandle @allowed {\n")
		var body strings.Builder
		if e.IsStatic() {
			renderStatic(&body, e, in.Root)
		} else {
			renderProxy(&body, e)
		}
		for line := range strings.SplitSeq(strings.TrimRight(body.String(), "\n"), "\n") {
			fmt.Fprintf(&b, "\t%s\n", line)
		}
		b.WriteString("\t}\n\thandle {\n\t\tabort\n\t}\n")
	} else if e.IsStatic() {
		renderStatic(&b, e, in.Root)
	} else {
		renderProxy(&b, e)
	}

	if raw := strings.TrimRight(e.Raw, "\n"); raw != "" {
		b.WriteString("\n")
		writeRaw(&b, raw)
	}

	b.WriteString("}\n")
	return b.String(), nil
}

// siteAddress builds the Caddyfile site address line. Multiple domains are
// comma-separated; the path suffix, if any, applies to all of them.
func siteAddress(e *config.Expose) string {
	path := strings.TrimSuffix(e.Path, "/*")
	addrs := make([]string, 0, len(e.Domains))
	for _, d := range e.Domains {
		addrs = append(addrs, d+path)
	}
	return strings.Join(addrs, ", ")
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
