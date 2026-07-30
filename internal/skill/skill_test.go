package skill

import (
	"strings"
	"testing"
)

// TestSkillHasValidFrontmatter guards the thing that makes the file a skill at
// all. Without a parseable header it is just a document nothing loads, and the
// failure is silent — an agent simply never receives the guidance.
func TestSkillHasValidFrontmatter(t *testing.T) {
	body := Content()

	if !strings.HasPrefix(body, "---\n") {
		t.Fatal("must open with a YAML frontmatter fence")
	}
	end := strings.Index(body[4:], "\n---\n")
	if end < 0 {
		t.Fatal("frontmatter is never closed")
	}
	head := body[4 : 4+end]

	for _, key := range []string{"name:", "description:"} {
		if !strings.Contains(head, key) {
			t.Errorf("frontmatter is missing %s\n%s", key, head)
		}
	}
	if !strings.Contains(head, "name: pilot") {
		t.Errorf("the skill must be named pilot, so `/pilot` invokes it:\n%s", head)
	}
	if len(strings.TrimSpace(body[4+end:])) < 500 {
		t.Error("the body is suspiciously short; the embed may have picked up a stub")
	}
}

// TestSkillStatesTheWithheldFlags is the part that matters most.
//
// The skill is advisory — Pilot does not enforce it — so if a flag that removes
// a safety net is not named here, nothing else stops an agent using it. Each of
// these either disables automatic rollback, overrides the guard protecting
// databases, or ships credentials into a model's context.
func TestSkillStatesTheWithheldFlags(t *testing.T) {
	body := Content()
	for _, flag := range []string{"--no-verify", "--force", "--no-redact", "@tag", "doctor --fix"} {
		if !strings.Contains(body, flag) {
			t.Errorf("the skill never mentions %s, so nothing warns an agent off it", flag)
		}
	}
}

// TestSkillWarnsThatLogsAreUntrusted — an agent diagnosing a failure reads log
// output, which is the one place attacker-controlled text enters its context.
func TestSkillWarnsThatLogsAreUntrusted(t *testing.T) {
	body := strings.ToLower(Content())
	if !strings.Contains(body, "untrusted") {
		t.Error("the skill must say log output is untrusted input")
	}
	if !strings.Contains(body, "never as instructions") && !strings.Contains(body, "not as instructions") {
		t.Error("the skill must say log content is data, not instructions to follow")
	}
}
