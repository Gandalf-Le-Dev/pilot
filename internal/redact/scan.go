// Package redact finds credentials in text that was never meant to carry them.
//
// It exists because of a real leak: a wakapi service was logging every request
// URI, and those URIs carried the user's API key as a query parameter. The key
// was in `docker logs` on the host, in any backup of them, and in the output of
// anything that read them.
//
// The detection lives in its own package because two things need it and they
// must agree: `pilot doctor`, which reports the leak so the *cause* can be
// fixed, and log redaction, which cleans a single read path. Two copies of "what
// counts as a secret" would diverge, and the one that matters would be the
// out-of-date one.
package redact

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Kind is how a match was recognised. The distinction matters for how much to
// trust it.
type Kind string

const (
	// KindLabelled matched a parameter whose *name* says it holds a secret:
	// `api_key=…`, `password: …`. The name is the evidence, never the value.
	KindLabelled Kind = "labelled"

	// KindFormat matched a value whose format can only be a credential — a JWT,
	// an AWS access key ID, a PEM private key header. These are shapes, but
	// unambiguous ones.
	KindFormat Kind = "format"

	// KindKnown matched a value Pilot itself supplied to the service, so there
	// is no guessing involved at all.
	KindKnown Kind = "known"
)

// Match reports a credential found in text.
//
// It deliberately carries no part of the secret. A finding that quoted the value
// would copy it into whatever reads the finding — a terminal, a JSON document,
// a CI log — which is the problem, not the report of it.
type Match struct {
	Kind Kind `json:"kind"`

	// Label identifies what was found without revealing it: the parameter name,
	// the credential format, or the environment variable whose value appeared.
	Label string `json:"label"`

	// Count is how many times it occurred in the sampled text.
	Count int `json:"count"`
}

// labelHead matches `name=value` and `name: value` where the name says
// secret, up to the value: the name, then the separator with any quote that
// opens the value.
//
// Curated rather than clever. A bare `key=` is excluded because it is far too
// common in ordinary log output to be evidence of anything, and a check that
// cries wolf gets switched off.
// The separator is kept apart so redaction can put the value back as a
// placeholder while leaving `api_key=` intact. A log line that says a key was
// passed and does not say which is still useful for debugging; one with the
// whole pair removed is not.
var labelHead = regexp.MustCompile(`(?i)\b(api[_-]?keys?|apikey|access[_-]?token|refresh[_-]?token|` +
	`id[_-]?token|auth[_-]?token|bearer|token|passwords?|passwd|pwd|secrets?|client[_-]?secret|` +
	`private[_-]?key|secret[_-]?key|signature|sessionid|session[_-]?token|credentials?)` +
	`((?:\\*["'])?\s*[=:]\s*(?:\\*["'])?)`)

// minLabelledLength is the shortest value a labelled match reports.
const minLabelledLength = 4

// labelledValue is one value found after a labelHead.
type labelledValue struct {
	label      string
	start, end int
}

// findLabelled returns each labelled value in text, in order and without
// overlap. A name inside a value already taken is skipped only when its own
// value starts inside that one too: in `password="abc token:` followed by a
// line holding the token, the token is a value of its own.
func findLabelled(text string) []labelledValue {
	var out []labelledValue
	taken := 0
	for _, loc := range labelHead.FindAllStringSubmatchIndex(text, -1) {
		if loc[1] < taken {
			continue
		}
		start := loc[1]
		end := valueEnd(text, start, text[loc[4]:loc[5]])
		if utf8.RuneCountInString(text[start:end]) < minLabelledLength {
			continue
		}
		out = append(out, labelledValue{label: text[loc[2]:loc[3]], start: start, end: end})
		taken = end
	}
	return out
}

// valueEnd finds where the value starting at i ends. A value opened by a quote
// runs to the matching unescaped quote, spaces and commas included, so none of
// it is left behind. An unquoted value stops at the first character that
// cannot be part of one.
//
// A double quote never closed runs to the end of the line. A single quote
// never closed is more likely an apostrophe than an opener, as in
// `token: 'abc def` inside a JSON string, so its value is read as unquoted.
//
// A quote opened as `\"` sits in a string logged inside another string, such
// as a JSON request body, where every character arrives escaped once more;
// `\\\"` is one string further in. The value is read that many layers of
// escapes down, and ends where an enclosing string does if its own quote
// never comes.
func valueEnd(s string, i int, sep string) int {
	q := sep[len(sep)-1]
	if q != '"' && q != '\'' {
		return unquotedEnd(s, i)
	}
	depth, ok := escapeDepth(len(sep) - 1 - len(strings.TrimRight(sep[:len(sep)-1], `\`)))
	if !ok {
		return i
	}
	start := i
	escaped := false
	for {
		c, next, end := read(s, i, depth, q)
		switch {
		case end == lineEnd && q == '\'':
			return unquotedEnd(s, start)
		case end != reading:
			return i
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == q:
			return i
		}
		i = next
	}
}

func unquotedEnd(s string, i int) int {
	for i < len(s) && !strings.ContainsRune("\"'&,;} \t\n\f\r", rune(s[i])) {
		i++
	}
	return i
}

// escapeDepth is how many strings deep a quote preceded by n backslashes
// sits: each layer escapes the backslashes of the one inside it too, so n is
// 0, 1, 3, 7 and so on. Any other n is not a quote that opens a value.
func escapeDepth(n int) (int, bool) {
	depth := 0
	for ; n > 0; n = (n - 1) / 2 {
		if n%2 == 0 {
			return 0, false
		}
		depth++
	}
	return depth, true
}

// readEnd says why read stopped, if it did.
type readEnd int

const (
	reading readEnd = iota
	lineEnd
	enclosingEnd
)

// read returns the character at i as seen depth layers of escapes down, where
// a backslash and the character after it read as that character, and the
// index after it. It stops at the end of the line, or at a bare quote q in a
// shallower layer, where an enclosing string ends.
func read(s string, i, depth int, q byte) (c byte, next int, end readEnd) {
	if i >= len(s) || s[i] == '\n' {
		return 0, i, lineEnd
	}
	if depth == 0 {
		return s[i], i + 1, reading
	}
	c, next, end = read(s, i, depth-1, q)
	if end != reading {
		return 0, i, end
	}
	if c == q {
		return 0, i, enclosingEnd
	}
	if c != '\\' {
		return c, next, reading
	}
	c, next, end = read(s, next, depth-1, q)
	if end != reading {
		return 0, i, end
	}
	return c, next, reading
}

// formats are values that cannot plausibly be anything but a credential.
//
// This is where shape-matching is legitimate, and the line is drawn carefully.
// Generic entropy scanning is rejected outright: Pilot's own release IDs, git
// SHAs, container IDs, and UUIDs are all high-entropy, so a threshold either
// floods the report with its own identifiers or misses real keys. A JWT header
// is never a release ID.
var formats = []struct {
	name string
	re   *regexp.Regexp
}{
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{4,}`)},
	{"AWS access key ID", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"PEM private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[baprse]-[A-Za-z0-9-]{10,}`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"Stripe key", regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{16,}\b`)},
}

// placeholders are values that look labelled but reveal nothing, so reporting
// them would be noise — and worse, would train someone to ignore the check.
var placeholders = map[string]bool{
	"": true, "-": true, "null": true, "nil": true, "none": true, "true": true,
	"false": true, "redacted": true, "hidden": true, "empty": true, "unset": true,
	"***": true, "****": true, "*****": true, "xxx": true, "xxxx": true,
	"<redacted>": true, "[redacted]": true, "changeme": true, "your_api_key": true,
}

// harmless reports whether a labelled match reveals nothing. A pwd that is a
// bare path is sudo's working directory; `=`, `?` or `:` mean a pair rode along.
// Some JSON encoders, PHP's among them, write `/` as `\/`; any other backslash
// is an escape that could hide one of those characters.
func harmless(label, raw string) bool {
	value := strings.Trim(raw, `"'`)
	if placeholders[strings.ToLower(value)] {
		return true
	}
	path := strings.ReplaceAll(value, `\/`, "/")
	return strings.EqualFold(label, "pwd") && strings.HasPrefix(path, "/") &&
		!strings.ContainsAny(path, `=?:\`)
}

// minKnownLength is the shortest supplied value worth searching for.
//
// A short secret is still a secret, but a two- or three-character value occurs
// in ordinary text constantly, and a check that reports every line is a check
// nobody reads.
const minKnownLength = 6

// Scan reports credentials found in text.
//
// known maps names to values Pilot supplied to the service — environment
// variables, typically — so a match can be reported by name without the value
// appearing anywhere. Pass nil when there is nothing to compare against.
//
// Results are sorted so output is stable between runs; an unstable report reads
// as a change when nothing changed.
func Scan(text string, known map[string]string) []Match {
	counts := map[Match]int{}

	for _, v := range findLabelled(text) {
		if harmless(v.label, text[v.start:v.end]) {
			continue
		}
		counts[Match{Kind: KindLabelled, Label: strings.ToLower(v.label)}]++
	}

	for _, f := range formats {
		if n := len(f.re.FindAllString(text, -1)); n > 0 {
			counts[Match{Kind: KindFormat, Label: f.name}] += n
		}
	}

	for name, value := range known {
		if len(value) < minKnownLength || strings.Contains(value, "${") {
			continue // too short to search for, or an unresolved reference
		}
		if n := strings.Count(text, value); n > 0 {
			counts[Match{Kind: KindKnown, Label: name}] += n
		}
	}

	out := make([]Match, 0, len(counts))
	for m, n := range counts {
		m.Count = n
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// Summarise renders matches as a short phrase for a human, naming labels and
// never values.
func Summarise(matches []Match) string {
	if len(matches) == 0 {
		return ""
	}
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		parts = append(parts, m.Label)
	}
	return strings.Join(parts, ", ")
}

// secretName matches environment variable names that hold credentials.
//
// The `known` layer needs this filter, and leaving it out produced an immediate
// false positive: every literal value in a service's environment was searched
// for in its logs, so `BASE_URL=https://kite.example.com` was reported as a
// leaked credential the moment kite logged its own base URL.
//
// The lesson is the one this package is built on — match the name, never the
// value. A variable is treated as secret because it is *called* a password, not
// because its contents look unusual.
var secretName = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|` +
	`private[_-]?key|credential|salt|signature|access[_-]?key|auth)`)

// IsSecretName reports whether an environment variable's name says it holds a
// credential.
//
// Names ending in _FILE, _PATH, or _URL are excluded: they hold a location, and
// a location appearing in a log line is ordinary rather than a leak.
func IsSecretName(name string) bool {
	upper := strings.ToUpper(name)
	for _, suffix := range []string{"_FILE", "_PATH", "_URL", "_URI", "_NAME", "_ID"} {
		if strings.HasSuffix(upper, suffix) {
			return false
		}
	}
	return secretName.MatchString(name)
}
