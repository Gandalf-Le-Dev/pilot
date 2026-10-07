package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

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

// CredentialName is the file the unit's LoadCredential= exposes under
// $CREDENTIALS_DIRECTORY.
const CredentialName = "server.yaml"

// LoadConfig reads and checks the server's configuration.
//
// Parsed strictly, like everything else the CLI writes to a host. The file is
// installed by the same agent sync that installs this binary, so a
// field this build does not know means the two were not installed together,
// and refusing to start says so where a default would hide it.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(raw)
}

// ParseConfig decodes and checks a configuration.
func ParseConfig(raw []byte) (Config, error) {
	var c Config
	if err := config.UnmarshalStrict(raw, &c); err != nil {
		return Config{}, err
	}

	if c.Title == "" {
		c.Title = "Status"
	}
	if c.SilentAfter.IsZero() {
		c.SilentAfter = config.DefaultSilentAfter
	}

	var problems []string
	if c.SilentAfter < 0 {
		problems = append(problems, "silent_after is negative")
	}
	if c.Listen != "" {
		switch ip := net.ParseIP(c.Listen); {
		case ip == nil:
			problems = append(problems, fmt.Sprintf("listen %q is not an IP address", c.Listen))
		case ip.IsUnspecified():
			problems = append(problems, fmt.Sprintf("listen %s would take reports on every interface", c.Listen))
		case ip.IsLoopback():
			problems = append(problems, "listen is a loopback address, which the server binds already")
		}
	}
	if len(c.Hosts) == 0 {
		problems = append(problems, "no hosts: nothing could report")
	}
	for _, name := range sortedKeys(c.Hosts) {
		if b, err := hex.DecodeString(c.Hosts[name].TokenSHA256); err != nil || len(b) != sha256.Size {
			problems = append(problems, fmt.Sprintf("hosts.%s.token_sha256 is not a sha256 hex digest", name))
		}
	}
	for _, name := range sortedKeys(c.Notifiers) {
		switch t := c.Notifiers[name].Type; {
		case !slices.Contains(alert.AllNotifierTypes, t):
			problems = append(problems, fmt.Sprintf("notifiers.%s has unknown type %q", name, t))
		case alert.NotifierType(t) == alert.TypeCommand:
			problems = append(problems, fmt.Sprintf("notifiers.%s is a command, which this sandboxed process cannot run", name))
		}
	}
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  %s", strings.Join(problems, "\n  "))
	}
	return c, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RefuseRoot stops the server running as root. Its unit runs it as a
// DynamicUser; root here means someone started it by hand, and the one
// process that listens beyond loopback is the last that should hold root.
func RefuseRoot(euid int) error {
	if euid == 0 {
		return fmt.Errorf("refusing to run as root: start it through pilot-server.service, which runs it as an unprivileged dynamic user")
	}
	return nil
}
