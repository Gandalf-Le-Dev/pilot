package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

var validHash = statuspage.TokenHash(statuspage.Token("s", "vps"))

const sampleConfig = `
title: mroc.me
listen: 100.64.0.10
silent_after: 2m
notifiers:
  discord: {type: discord, url: "https://discord.example/hook"}
hosts:
  vps:
    token_sha256: HASH
    services: {docmost: Notes, site: site}
`

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), CredentialName)
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(sampleConfig, "HASH", validHash)), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "mroc.me" || c.Listen != "100.64.0.10" || c.SilentAfter.Duration() != 2*time.Minute {
		t.Errorf("config = %+v", c)
	}
	if got := c.Hosts["vps"].Services["docmost"]; got != "Notes" {
		t.Errorf("label = %q", got)
	}
	if c.Notifiers["discord"].Endpoint() == "" {
		t.Error("notifier endpoint lost")
	}
}

func TestParseConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte("hosts:\n  vps: {token_sha256: " + validHash + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.SilentAfter != config.DefaultSilentAfter || c.Title == "" {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestParseConfigRejects(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"unknown field":   {strings.ReplaceAll(sampleConfig, "HASH", validHash) + "secret: oops\n", "unknown field"},
		"plain token":     {strings.ReplaceAll(sampleConfig, "HASH", "not-a-hash"), "hosts.vps.token_sha256"},
		"short hash":      {strings.ReplaceAll(sampleConfig, "HASH", "abcd"), "hosts.vps.token_sha256"},
		"listen hostname": {strings.ReplaceAll(strings.ReplaceAll(sampleConfig, "HASH", validHash), "100.64.0.10", "ks.ts.net"), "not an IP"},
		"no hosts":        {"title: x\n", "no hosts"},
		"listen anywhere": {strings.ReplaceAll(strings.ReplaceAll(sampleConfig, "HASH", validHash), "100.64.0.10", "0.0.0.0"), "every interface"},
		"listen loopback": {strings.ReplaceAll(strings.ReplaceAll(sampleConfig, "HASH", validHash), "100.64.0.10", "127.0.0.1"), "loopback"},
		"command":         {strings.ReplaceAll(strings.ReplaceAll(sampleConfig, "HASH", validHash), `{type: discord, url: "https://discord.example/hook"}`, "{type: command, command: [logger]}"), "command"},
		"notifier type":   {strings.ReplaceAll(strings.ReplaceAll(sampleConfig, "HASH", validHash), "type: discord", "type: pager"), "unknown type"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestRefuseRoot(t *testing.T) {
	if err := RefuseRoot(0); err == nil {
		t.Error("root was allowed to start the server")
	}
	if err := RefuseRoot(61234); err != nil {
		t.Errorf("an unprivileged user was refused: %v", err)
	}
}

// The tailnet address can be missing at boot; the bind keeps trying instead
// of taking the page down with it.
func TestListenRetryWaitsForTheAddress(t *testing.T) {
	attempts := 0
	listen := func(addr string) (net.Listener, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("bind: cannot assign requested address")
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
	ln, err := listenRetry(context.Background(), "100.64.0.10:7381", time.Millisecond, listen)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never := func(string) (net.Listener, error) { return nil, errors.New("still missing") }
	if _, err := listenRetry(ctx, "100.64.0.10:7381", time.Hour, never); err == nil {
		t.Error("a cancelled retry should give up")
	}
}
