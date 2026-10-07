package server

import "github.com/Gandalf-Le-Dev/pilot/internal/config"

// Config is everything the status server knows, written by the CLI on the
// server host and handed to the unit as a credential.
//
// It holds token hashes rather than tokens, and notifier endpoints already
// resolved, so the server needs neither the fleet secret nor the operator's
// keychain.
type Config struct {
	Title string `yaml:"title"`

	// Listen is the tailnet address the ingest listener binds beside
	// loopback.
	Listen string `yaml:"listen"`

	SilentAfter config.Duration            `yaml:"silent_after"`
	Notifiers   map[string]config.Notifier `yaml:"notifiers"`
	Hosts       map[string]HostConfig      `yaml:"hosts"`
}

// HostConfig is one reporting host.
type HostConfig struct {
	// TokenSHA256 is the hex sha256 of the host's bearer token.
	TokenSHA256 string `yaml:"token_sha256"`

	// Services maps each service the host reports to its public label. A
	// reported service missing here is dropped, so the page lists exactly
	// what the CLI decided, whatever an agent sends.
	Services map[string]string `yaml:"services"`
}
