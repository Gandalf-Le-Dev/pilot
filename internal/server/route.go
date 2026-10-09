package server

import (
	"fmt"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

// SnippetName names the page's route among the services' routes. Config
// validation reserves the leading underscore, so no service can claim it.
const SnippetName = "_status"

// Route renders the Caddy site block that fronts the page on the status host.
//
// It is a service route in every respect but origin, so it goes through the
// same renderer: same retry behaviour, same bind for a host whose Caddyfile
// binds explicitly. Only the header changes, because "change
// services/_status.yaml instead" would send the reader looking for a file
// that does not exist.
func Route(f *config.Fleet) (string, error) {
	st := f.Status
	if st == nil {
		return "", fmt.Errorf("the fleet has no status block")
	}
	out, err := caddy.Render(caddy.Input{
		Service: SnippetName,
		Expose:  &config.Expose{Domains: []string{st.Domain}, Upstream: statuspage.PublicPort},
		Bind:    f.CaddyBindFor([]string{st.Host}),
	})
	if err != nil {
		return "", err
	}
	return strings.Replace(out,
		"change services/"+SnippetName+".yaml instead",
		"change the status block in "+config.FleetFile+" instead", 1), nil
}
