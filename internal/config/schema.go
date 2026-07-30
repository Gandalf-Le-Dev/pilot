package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// SchemaFingerprint is a digest of the configuration schema the agent parses.
//
// It exists because of a real failure. `expose.allow` was added to the config
// without changing proto.Version, so the handshake passed — the CLI and the
// agent agreed on the wire format — and then the agent rejected the payload
// sent over it with `unknown field "allow"`, mid-deploy, on a host that had
// already been staged. Recovering meant hand-building an agent and installing
// it with a flag, which is exactly the kind of escape hatch that lets the real
// problem survive.
//
// Strict parsing is not the bug and is not up for negotiation: an agent that
// quietly ignored `allow` would serve a route without its IP restriction, and
// a deploy would report success while opening a service to the internet. The
// bug was that nothing forced the version to move. Pinning this digest in a
// test does that mechanically, so the decision is made when the field is added
// rather than discovered when a deploy fails.
//
// The digest covers every field the agent decodes: names, paths, and types.
// Renaming a field, changing its type, or adding one all move it.
func SchemaFingerprint() string {
	sum := sha256.Sum256([]byte(strings.Join(SchemaFields(), "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// SchemaFields returns the fingerprint's input: one `path type` line per
// decodable field, sorted.
//
// Exported so the test that pins the fingerprint can diff against a recorded
// copy and name the field that moved. "The schema changed, bump the version"
// is an instruction; "you added expose.allow" is an explanation.
func SchemaFields() []string {
	var lines []string
	// Every schema that travels from the CLI to an agent: the fleet file, a
	// service definition, and the host-wide config pushed for alerting. Each is
	// parsed strictly on the far side, so a change to any of them can be
	// rejected mid-deploy.
	for _, root := range []any{Fleet{}, Service{}, FleetConfig{}} {
		t := reflect.TypeOf(root)
		walkSchema(t, strings.ToLower(t.Name()), map[reflect.Type]bool{}, &lines)
	}
	sort.Strings(lines)
	return lines
}

// walkSchema records every decodable field under prefix.
//
// onPath guards against a type that contains itself; it is keyed by type and
// unwound on the way out, so the same type appearing at two different paths is
// recorded at both rather than skipped at the second.
func walkSchema(t reflect.Type, prefix string, onPath map[reflect.Type]bool, lines *[]string) {
	t = deref(t)
	if t.Kind() != reflect.Struct || onPath[t] {
		return
	}
	onPath[t] = true
	defer delete(onPath, t)

	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: never decoded
		}

		name, inline, skip := yamlName(f)
		if skip {
			continue
		}

		path := prefix + "." + name
		if inline {
			// An inlined struct contributes its fields at this level, so the
			// path must not gain a segment — otherwise moving a field into or
			// out of an inline struct would change the fingerprint without
			// changing the YAML anyone writes.
			path = prefix
		} else {
			*lines = append(*lines, fmt.Sprintf("%s %s", path, typeName(f.Type)))
		}
		walkSchema(f.Type, path, onPath, lines)
	}
}

// yamlName reports how a field appears in YAML.
func yamlName(f reflect.StructField) (name string, inline, skip bool) {
	tag, ok := f.Tag.Lookup("yaml")
	if !ok {
		return strings.ToLower(f.Name), false, false
	}

	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, opt := range parts[1:] {
		if opt == "inline" {
			inline = true
		}
	}
	switch {
	case name == "-":
		return "", false, true
	case name == "":
		name = strings.ToLower(f.Name)
	}
	return name, inline, false
}

// typeName describes a field's shape, so a string becoming an int moves the
// fingerprint even though the field name did not change.
func typeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + typeName(t.Elem())
	case reflect.Slice, reflect.Array:
		return "[]" + typeName(t.Elem())
	case reflect.Map:
		return "map[" + typeName(t.Key()) + "]" + typeName(t.Elem())
	case reflect.Struct:
		return "struct"
	default:
		return t.Kind().String()
	}
}

func deref(t reflect.Type) reflect.Type {
	for {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
		default:
			return t
		}
	}
}
