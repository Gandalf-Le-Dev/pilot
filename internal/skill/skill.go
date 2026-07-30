// Package skill carries the agent instructions shipped with Pilot.
//
// Embedded in the binary rather than fetched or copy-pasted from documentation,
// so the guidance an agent follows and the behaviour it describes are always the
// same version. A skill sitting in someone's own config directory rots silently
// the moment Pilot changes — which is the third time this project has met that
// problem, after the CLI/agent protocol and the configuration schema, and the
// two before it were both found by a deploy failing rather than a test.
package skill

import _ "embed"

//go:embed SKILL.md
var content string

// Content is the skill, as Markdown with YAML frontmatter.
func Content() string { return content }
