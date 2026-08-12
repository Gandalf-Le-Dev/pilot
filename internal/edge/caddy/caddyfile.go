package caddy

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// ImportState describes whether the global Caddyfile is wired up to load
// Pilot's snippet directory.
type ImportState int

const (
	// ImportPresent means the import directive is already there. Appending
	// again would be a no-op.
	ImportPresent ImportState = iota

	// ImportMissing means the directive is absent and can be safely appended.
	ImportMissing

	// ImportUnsafe means the file uses the brace-less single-site form, where
	// appending would nest the import inside that site block rather than
	// applying it to the server. Pilot refuses to guess here.
	ImportUnsafe
)

func (s ImportState) String() string {
	switch s {
	case ImportPresent:
		return "present"
	case ImportMissing:
		return "missing"
	case ImportUnsafe:
		return "unsafe"
	}
	return "unknown"
}

// UnsafeHint is the remedy Pilot offers when it finds the brace-less form.
const UnsafeHint = "wrap the existing site in braces (`example.com { ... }`) so the import applies to the server, not to that one site"

// ImportDirective returns the line Pilot wants in the global Caddyfile.
//
// Caddy resolves relative import paths against the importing file, so when the
// snippet directory sits alongside the Caddyfile we emit the short relative
// form that operators recognise.
func ImportDirective(caddyfilePath, snippetDir string) string {
	return "import " + importTarget(caddyfilePath, snippetDir)
}

func importTarget(caddyfilePath, snippetDir string) string {
	dir := filepath.ToSlash(filepath.Dir(caddyfilePath))
	clean := filepath.ToSlash(filepath.Clean(snippetDir))
	if rel, err := filepath.Rel(dir, clean); err == nil {
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, "..") && rel != "." {
			return path.Join(rel, "*.caddy")
		}
	}
	return path.Join(clean, "*.caddy")
}

// Inspect reports whether content already imports the snippet directory, and
// whether it is safe to append the directive if not.
func Inspect(content, caddyfilePath, snippetDir string) (ImportState, string) {
	targets := map[string]bool{
		importTarget(caddyfilePath, snippetDir):                            true,
		path.Join(filepath.ToSlash(filepath.Clean(snippetDir)), "*.caddy"): true,
	}

	depth := 0
	for i, raw := range strings.Split(content, "\n") {
		line, delta := scanLine(raw)
		trimmed := strings.TrimSpace(line)
		start := depth
		depth += delta

		if trimmed == "" {
			continue
		}

		fields := strings.Fields(trimmed)
		if fields[0] == "import" && len(fields) > 1 {
			if targets[strings.Trim(fields[1], `"'`)] {
				return ImportPresent, ""
			}
			continue // some other import; harmless at any depth
		}

		// A top-level line that carries content but opens no block can only be
		// a directive belonging to an implicit, brace-less site.
		if start == 0 && delta == 0 {
			return ImportUnsafe, fmt.Sprintf(
				"%s:%d: %q is a top-level directive, so this is a brace-less single-site Caddyfile",
				filepath.Base(caddyfilePath), i+1, trimmed)
		}
	}

	return ImportMissing, ""
}

// scanLine strips a Caddyfile comment from a line and reports the net change in
// block depth, ignoring braces inside quotes. Placeholders like {path} are
// balanced within a line, so they contribute nothing to the net.
func scanLine(raw string) (line string, delta int) {
	var b strings.Builder
	inQuote := false
	var quote byte

	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inQuote {
			if c == '\\' && i+1 < len(raw) {
				b.WriteByte(c)
				i++
				b.WriteByte(raw[i])
				continue
			}
			if c == quote {
				inQuote = false
			}
			b.WriteByte(c)
			continue
		}

		switch c {
		case '"', '\'':
			inQuote, quote = true, c
		case '#':
			// A comment starts at the beginning of a line or after whitespace.
			if i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t' {
				return b.String(), delta
			}
		case '{':
			delta++
		case '}':
			delta--
		}
		b.WriteByte(c)
	}
	return b.String(), delta
}

// BindUsage reports how a Caddyfile (or snippet) uses listener addresses:
// whether any site binds explicitly, and whether a global default_bind covers
// the sites that do not.
//
// The distinction matters because the two directives have opposite
// consequences for generated routes. A site-level `bind` splits Caddy into
// servers by listen address, and a specific address wins that address's
// traffic over the :443 wildcard — so a generated block without the same bind
// is unreachable from outside. A global `default_bind` instead applies to
// every site that doesn't bind, generated ones included, and closes the gap.
func BindUsage(content string) (siteBind, defaultBind bool) {
	for raw := range strings.SplitSeq(content, "\n") {
		line, _ := scanLine(raw)
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "bind":
			siteBind = true
		case "default_bind":
			defaultBind = true
		}
	}
	return siteBind, defaultBind
}

// AppendImport returns content with the import directive appended. It is the
// caller's job to have checked Inspect first; appending to an ImportUnsafe file
// is refused here as a backstop.
func AppendImport(content, caddyfilePath, snippetDir string) (string, error) {
	state, reason := Inspect(content, caddyfilePath, snippetDir)
	switch state {
	case ImportPresent:
		return content, nil
	case ImportUnsafe:
		return "", fmt.Errorf("refusing to edit %s: %s\n%s", caddyfilePath, reason, UnsafeHint)
	}

	var b strings.Builder
	b.WriteString(content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	if strings.TrimSpace(content) != "" {
		b.WriteString("\n")
	}
	b.WriteString("# added by pilot — loads one generated route per service\n")
	b.WriteString(ImportDirective(caddyfilePath, snippetDir))
	b.WriteString("\n")
	return b.String(), nil
}
