package doctor

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	// A published port: `- "8085:8080"`, `- "127.0.0.1:8085:8080"`, or
	// `- "${BIND_TS}:8081:8081"`. The host portion is deliberately matched
	// loosely rather than as an address — the first version only accepted digits
	// and dots, and so missed the interpolated form, which is the one that broke
	// a real deploy.
	//
	// The captured group is the host port. That is what matters: two colours run
	// at once and cannot both bind it.
	publishedPort = regexp.MustCompile(`(?m)^\s*-\s*["']?(?:[^:"'\s]+:)?(\d+):\d+`)

	// A top-level `volumes:` key declares named volumes. Compose scopes those
	// per project, so each colour gets its own.
	namedVolumes = regexp.MustCompile(`(?m)^volumes:\s*$`)
)

// checkBlueGreen reports compose files that cannot survive a blue-green rollout.
//
// Both problems below were found by switching a real service to blue-green and
// watching what happened, and neither is visible in the configuration on its
// own — they only appear when two colours exist at once, which is to say during
// the deploy you were hoping would be safe.
func checkBlueGreen(ctx context.Context, env *Env) []Finding {
	var out []Finding

	for _, name := range env.Fleet.ServiceNames() {
		s := env.Fleet.Services[name]
		if s.Compose == nil || s.Rollout == nil || !s.Rollout.IsBlueGreen() {
			continue
		}

		body, err := readComposeFile(env.Fleet.Root, s)
		if err != nil {
			continue
		}
		text := string(body)

		// Named volumes are scoped to the compose project, and each colour is a
		// separate project. The green stack therefore starts with an empty
		// volume, and after the flip whatever the service had stored is on the
		// other colour. Nothing errors; the data is simply not there.
		if namedVolumes.MatchString(text) {
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeConfig,
				Title: fmt.Sprintf("%s uses blue-green with named volumes", name),
				Detail: "Compose scopes a named volume to its project and each colour is a separate " +
					"project, so the new colour starts with an empty volume and the old data stays " +
					"behind on the other one. The deploy succeeds and the service comes up blank.",
				Hint: "use a bind mount to a fixed host path, or `strategy: recreate` for a service that stores data",
			})
		}

		// Every published host port is contended: the old colour still holds it
		// while the new one starts. Pilot manages the ports it flips and knows
		// nothing about the rest.
		if extra := extraPorts(text, s.Rollout.Ports); len(extra) > 0 {
			out = append(out, Finding{
				Status: StatusFail, Scope: ScopeConfig,
				Title: fmt.Sprintf("%s publishes host port %s, which both colours would bind",
					name, strings.Join(extra, ", ")),
				Detail: "A blue-green deploy runs both colours at once. Pilot assigns the ports it " +
					"flips, but any other published port is held by the running colour, so the new " +
					"one fails to start.",
				Hint: "remove the fixed mapping, or expose the port only through the ports Pilot assigns",
			})
		}
	}
	return out
}

// extraPorts lists published host ports that are not the rollout's own.
func extraPorts(text string, assigned []int) []string {
	own := map[string]bool{}
	for _, p := range assigned {
		own[fmt.Sprint(p)] = true
	}

	seen := map[string]bool{}
	var out []string
	for _, m := range publishedPort.FindAllStringSubmatch(text, -1) {
		port := m[1]
		if own[port] || seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	sort.Strings(out)
	return out
}
