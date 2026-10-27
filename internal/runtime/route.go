package runtime

import (
	"context"
	"fmt"
	"path"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/edge/caddy"
)

// ActiveColor reports which colour serves a blue-green service. A service
// with no state serves blue; a state that cannot be read is an error, since
// guessing a colour would compare routes against the wrong one or flip onto
// the stack that serves.
func ActiveColor(ctx context.Context, t *Target) (config.Color, error) {
	st, err := ReadState(ctx, t)
	if err != nil {
		return "", err
	}
	return config.ServingColor(st.ActiveColor), nil
}

// ExpectedRoute is the route t.Release should have installed, on a host whose
// snippet dir is snippetDir and whose caddy.bind is bind.
//
// For blue-green it is the serving colour's: the release's copy for that
// colour, or for a release staged before copies were kept, the same render
// the agent installs, so that a rollback to one reads as no drift.
func ExpectedRoute(ctx context.Context, t *Target, snippetDir string, bind []string) (string, error) {
	if !t.Service.Rollout.IsBlueGreen() {
		b, err := t.Client.ReadFile(ctx, t.Layout.Snippet(t.Service.Name, t.Release))
		return string(b), err
	}
	color, err := ActiveColor(ctx, t)
	if err != nil {
		return "", err
	}
	if b, err := t.Client.ReadFile(ctx, t.Layout.ColorSnippet(t.Service.Name, t.Release, string(color))); err == nil {
		return string(b), nil
	}
	return RenderColorRoute(ctx, t, snippetDir, bind, color)
}

// RenderColorRoute renders a blue-green service's route for one colour, at
// that colour's host port, for a release staged before the plan kept a copy
// per colour. It is the one render both the agent's install and every
// comparison use for such a release.
//
// It cannot see the other services, so it renders a shared route when the
// release's own route is shared, or when one of the domains already has a
// shared site block on the host: a block of its own beside it is refused.
func RenderColorRoute(ctx context.Context, t *Target, snippetDir string, bind []string, c config.Color) (string, error) {
	svc := t.Service
	if svc.Expose == nil {
		return "", fmt.Errorf("service %q has no route to flip", svc.Name)
	}
	scoped := *svc.Expose
	scoped.Upstream = svc.Rollout.PortFor(c)

	shared := false
	if b, err := t.Client.ReadFile(ctx, t.Layout.Snippet(svc.Name, t.Release)); err == nil && caddy.Shared(string(b)) {
		shared = true
	}
	for _, d := range svc.Expose.Domains {
		if _, err := t.Client.ReadFile(ctx, path.Join(snippetDir, caddy.SiteFile(d))); err == nil {
			shared = true
		}
	}

	return caddy.Render(caddy.Input{
		Service: svc.Name,
		Expose:  &scoped,
		Root:    t.Layout.Current(svc.Name),
		Bind:    bind,
		Shared:  shared,
	})
}
