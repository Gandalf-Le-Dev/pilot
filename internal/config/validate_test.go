package config

import "testing"

// TestNotifierAcceptsSecretReference — validation runs before resolution, so a
// reference must not be judged as a URL. Rejecting it would force webhooks,
// which are credentials, to be written literally into the fleet repository.
func TestNotifierAcceptsSecretReference(t *testing.T) {
	f := &Fleet{
		Hosts: map[string]*Host{"web": {Name: "web", Address: "web.example.com"}},
		Notifiers: map[string]Notifier{
			"chat": {Type: "discord", Webhook: "${cmd:security find-generic-password -s x -w}"},
			"page": {Type: "ntfy", URL: "${env:NTFY_URL}"},
		},
	}

	var ds Diagnostics
	validateNotifiers(f, &ds)

	for _, d := range ds.Sorted() {
		t.Errorf("%s: %s: %s", d.File, d.Field, d.Message)
	}
}

// TestNotifierStillRejectsRealGarbage keeps the exemption narrow: a value with
// no reference in it is still checked.
func TestNotifierStillRejectsRealGarbage(t *testing.T) {
	f := &Fleet{
		Hosts:     map[string]*Host{"web": {Name: "web", Address: "web.example.com"}},
		Notifiers: map[string]Notifier{"chat": {Type: "discord", Webhook: "not-a-url"}},
	}

	var ds Diagnostics
	validateNotifiers(f, &ds)
	if !ds.HasErrors() {
		t.Error("a malformed literal URL must still be reported")
	}
}
