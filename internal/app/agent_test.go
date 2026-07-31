package app

import (
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

// TestNotifierSecretsAreResolved covers the reason this exists: a Discord or
// Slack webhook is a credential, and without resolution the only way to
// configure one is to write it literally into the fleet repository.
func TestNotifierSecretsAreResolved(t *testing.T) {
	t.Setenv("PILOT_TEST_WEBHOOK", "https://discord.example/api/webhooks/123/s3cr3t")

	got, err := resolveNotifiers(map[string]config.Notifier{
		"chat": {Type: "discord", Webhook: "${env:PILOT_TEST_WEBHOOK}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["chat"].Webhook != "https://discord.example/api/webhooks/123/s3cr3t" {
		t.Errorf("webhook = %q, want it resolved", got["chat"].Webhook)
	}
}

func TestNotifierCommandArgsAreResolved(t *testing.T) {
	t.Setenv("PILOT_TEST_TOKEN", "tok-abc123")

	got, err := resolveNotifiers(map[string]config.Notifier{
		"page": {Type: "command", Command: []string{"curl", "-H", "Authorization: Bearer ${env:PILOT_TEST_TOKEN}"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "Authorization: Bearer tok-abc123"; got["page"].Command[2] != want {
		t.Errorf("command[2] = %q, want %q", got["page"].Command[2], want)
	}
}

// TestUnresolvableNotifierFails — passing the literal through would configure a
// notifier that posts to a URL containing "${cmd:...}", which fails at the worst
// possible moment: when something is already wrong and the alert cannot be sent.
func TestUnresolvableNotifierFails(t *testing.T) {
	_, err := resolveNotifiers(map[string]config.Notifier{
		"chat": {Type: "discord", Webhook: "${env:PILOT_DEFINITELY_NOT_SET_ANYWHERE}"},
	})
	if err == nil {
		t.Fatal("want an error rather than a literal ${env:...} shipped as a URL")
	}
	if !strings.Contains(err.Error(), "chat") {
		t.Errorf("error should name the notifier: %v", err)
	}
}

func TestPlainNotifiersAreUntouched(t *testing.T) {
	in := map[string]config.Notifier{
		"phone": {Type: "ntfy", URL: "https://ntfy.sh/some-topic"},
	}
	got, err := resolveNotifiers(in)
	if err != nil {
		t.Fatal(err)
	}
	if got["phone"].URL != in["phone"].URL {
		t.Errorf("URL = %q, want it unchanged", got["phone"].URL)
	}
}
