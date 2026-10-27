// Package compose implements the Runtime for docker compose stacks.
//
// The pinned project name is what makes a deploy an update rather than a second
// copy: compose diffs the new file against the containers already labelled with
// that project and recreates only what changed.
package compose

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

// Runtime implements runtime.Runtime for compose services.
type Runtime struct{}

func New() *Runtime { return &Runtime{} }

func (r *Runtime) Kind() config.Runtime { return config.RuntimeCompose }

// project is the compose project name, pinned to the service so containers are
// replaced rather than duplicated across releases.
func project(t *runtime.Target) string {
	if t.Service.Compose != nil && t.Service.Compose.Project != "" {
		return t.Service.Compose.Project
	}
	return t.Service.Name
}

// composeFile is the rendered compose file inside a release directory. Pilot
// always stages it under a fixed name, whatever it was called in the repo.
const composeFile = "compose.yaml"

// cmd builds a `docker compose` invocation against a specific release
// directory. Running from within the release means relative paths and bind
// mounts in the compose file resolve the way the author intended.
func cmd(dir, proj string, args ...string) string {
	base := []string{
		"docker", "compose",
		"--project-name", proj,
		"--project-directory", dir,
		"--file", path.Join(dir, composeFile),
	}
	// Unconditional: StageCommon writes the .env for every compose release,
	// even an empty one, precisely so this flag can never point at a missing
	// file.
	base = append(base, "--env-file", path.Join(dir, release.EnvFile))
	return transport.Join(append(base, args...)...)
}

// Stage ships the release and pulls its images, so that Activate has nothing
// left to do that could fail slowly or need the network.
func (r *Runtime) Stage(ctx context.Context, t *runtime.Target, in runtime.StageInput) error {
	if err := runtime.StageCommon(ctx, t, in); err != nil {
		return err
	}

	relDir := t.ReleaseDir()
	proj := project(t)

	// Validate before pulling: a malformed compose file should fail in a
	// second rather than after a two-minute image download.
	res, err := t.Client.Run(ctx, cmd(relDir, proj, "config", "--quiet"))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("compose file for %s is invalid: %w", t.Label(), res.Err())
	}

	res, err = t.Client.Run(ctx, cmd(relDir, proj, "pull", "--quiet"))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("pulling images for %s: %w", t.Label(), res.Err())
	}

	// Both colors' overrides are written now, so either can be started later
	// from this same release. That is what makes a rollback after the flip a
	// matter of starting the other color rather than re-staging.
	return StageColor(ctx, t)
}

// Activate swaps the symlink and reconciles the stack against the new file.
//
// Blue-green does not come through here: it needs to start one colour beside
// the other and move the route, which is a sequence only the agent can run.
// Refusing is the honest failure — a plain `up -d` would create a third,
// unrouted stack alongside the two real ones.
func (r *Runtime) Activate(ctx context.Context, t *runtime.Target) error {
	if t.Service.Rollout.IsBlueGreen() {
		return fmt.Errorf("service %q uses blue-green, which must be activated by the agent on %s\n"+
			"install one with `pilot bootstrap %s`", t.Service.Name, t.Host.Name, t.Host.Name)
	}
	if err := runtime.Swap(ctx, t); err != nil {
		return err
	}

	// Run from `current` rather than the release path, so the containers'
	// recorded working directory follows the symlink and a later rollback
	// doesn't leave them pointing at a GC'd directory.
	res, err := t.Client.Run(ctx, cmd(t.CurrentDir(), project(t), "up", "--detach", "--remove-orphans", "--wait=false"))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("starting %s: %w", t.Label(), res.Err())
	}
	return nil
}

// Deactivate stops the stack, leaving its releases and volumes in place.
func (r *Runtime) Deactivate(ctx context.Context, t *runtime.Target) error {
	res, err := t.Client.Run(ctx, activeArgs(ctx, t, "down", "--remove-orphans"))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("stopping %s: %w", t.Label(), res.Err())
	}
	return nil
}

// Observe reports container-level state for the project.
func (r *Runtime) Observe(ctx context.Context, t *runtime.Target) (runtime.Observation, error) {
	obs := runtime.Observation{State: runtime.StateUnknown}

	current, err := runtime.ReadCurrent(ctx, t)
	if err != nil {
		return obs, err
	}
	obs.Release = current

	res, err := t.Client.Run(ctx, transport.Join("docker", "compose",
		"--project-name", ActiveProject(ctx, t), "ps", "--all", "--format", "json"))
	if err != nil {
		return obs, err
	}
	if !res.OK() {
		obs.Detail = strings.TrimSpace(res.Stderr)
		return obs, nil
	}

	instances, err := containers(ctx, t, res.Stdout)
	if err != nil {
		return obs, fmt.Errorf("reading container status for %s: %w", t.Label(), err)
	}
	obs.Instances = instances
	obs.State, obs.Detail = summarise(instances, current)
	obs.Since = serviceSince(instances)
	return obs, nil
}

// summarise reduces per-container states to one verdict for the service.
func summarise(instances []runtime.Instance, current string) (runtime.State, string) {
	if current == "" {
		return runtime.StateStopped, "no release is activated"
	}
	if len(instances) == 0 {
		return runtime.StateStopped, "no containers for this project"
	}

	var running, unhealthy, stopped, restarting int
	for _, in := range instances {
		switch strings.ToLower(in.State) {
		case "running":
			if strings.EqualFold(in.Health, "unhealthy") {
				unhealthy++
			} else {
				running++
			}
		case "restarting":
			restarting++
		default:
			stopped++
		}
	}

	switch {
	case unhealthy > 0:
		return runtime.StateDegraded, fmt.Sprintf("%d of %d containers unhealthy", unhealthy, len(instances))
	case restarting > 0:
		return runtime.StateDegraded, fmt.Sprintf("%d of %d containers restarting", restarting, len(instances))
	case stopped == len(instances):
		return runtime.StateStopped, "all containers stopped"
	case stopped > 0:
		return runtime.StateDegraded, fmt.Sprintf("%d of %d containers not running", stopped, len(instances))
	}
	return runtime.StateRunning, ""
}

// serviceSince is the earliest start among running containers, so an old
// one-shot that exited long ago does not age a stack started today. Only when
// nothing runs does it fall back to the earliest stop.
func serviceSince(instances []runtime.Instance) time.Time {
	anyRunning := slices.ContainsFunc(instances, func(in runtime.Instance) bool {
		return strings.EqualFold(in.State, "running")
	})
	var out time.Time
	for _, in := range instances {
		if in.Since.IsZero() || anyRunning && !strings.EqualFold(in.State, "running") {
			continue
		}
		if out.IsZero() || in.Since.Before(out) {
			out = in.Since
		}
	}
	return out
}

// psEntry mirrors the fields of `docker compose ps --format json` that Pilot
// uses. Compose has changed this shape between minor versions, so unknown
// fields are ignored and every field is treated as optional.
type psEntry struct {
	ID         string `json:"ID"`
	Name       string `json:"Name"`
	Service    string `json:"Service"`
	Image      string `json:"Image"`
	State      string `json:"State"`
	Health     string `json:"Health"`
	Status     string `json:"Status"`
	ExitCode   int    `json:"ExitCode"`
	RunningFor string `json:"RunningFor"`
}

// ParsePS reads `docker compose ps --format json`, which emits either a JSON
// array or one object per line depending on the compose version. Both are
// accepted rather than pinning users to a particular Docker release.
func ParsePS(raw []byte) ([]runtime.Instance, error) {
	instances, _, err := parsePS(raw)
	return instances, err
}

// parsePS also returns, for each instance, the ID or name docker inspect knows
// the container by.
func parsePS(raw []byte) ([]runtime.Instance, []string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil, nil
	}

	var entries []psEntry
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
			return nil, nil, err
		}
	} else {
		for line := range strings.SplitSeq(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e psEntry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				return nil, nil, fmt.Errorf("unparseable container status %q: %w", line, err)
			}
			entries = append(entries, e)
		}
	}

	out := make([]runtime.Instance, 0, len(entries))
	refs := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name
		if name == "" {
			name = e.Service
		}
		in := runtime.Instance{
			Name:   name,
			State:  e.State,
			Health: e.Health,
			Image:  e.Image,
			Detail: e.Status,
		}
		if in.State == "" {
			in.State = stateFromStatus(e.Status)
		}
		if e.ExitCode != 0 && !strings.EqualFold(in.State, "running") {
			in.Detail = strings.TrimSpace(fmt.Sprintf("%s (exit %d)", e.Status, e.ExitCode))
		}
		out = append(out, in)
		refs = append(refs, cmp.Or(e.ID, e.Name))
	}
	return out, refs, nil
}

// containers parses `docker compose ps` output and dates each container. ps
// only has CreatedAt, which survives restarts and reboots, so the last start or
// stop comes from one docker inspect over the whole project.
func containers(ctx context.Context, t *runtime.Target, psOut string) ([]runtime.Instance, error) {
	instances, refs, err := parsePS([]byte(psOut))
	if err != nil {
		return nil, err
	}
	setSince(ctx, t, instances, refs)
	return instances, nil
}

// setSince leaves Since zero when inspect fails: an age is cosmetic, and a
// failed observation is not. A non-zero exit is not a failure on its own:
// inspect still prints every container it found, so one removed since ps
// should not blank the others.
func setSince(ctx context.Context, t *runtime.Target, instances []runtime.Instance, refs []string) {
	named := slices.DeleteFunc(slices.Clone(refs), func(ref string) bool { return ref == "" })
	if len(named) == 0 {
		return
	}
	args := append([]string{"docker", "inspect", "--type", "container", "--format", inspectFormat}, named...)
	res, err := t.Client.Run(ctx, transport.Join(args...))
	if err != nil {
		return
	}
	entries, err := parseInspect([]byte(res.Stdout))
	if err != nil {
		return
	}
	for i, ref := range refs {
		if j := slices.IndexFunc(entries, func(e inspectEntry) bool { return e.is(ref) }); j >= 0 {
			instances[i].Since = entries[j].since()
		}
	}
}

// inspectFormat prints one JSON object per container, so names and times never
// have to be split on spaces.
const inspectFormat = `{"id":{{json .Id}},"name":{{json .Name}},"running":{{json .State.Running}},` +
	`"started":{{json .State.StartedAt}},"finished":{{json .State.FinishedAt}}}`

type inspectEntry struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Running  bool      `json:"running"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}

// is reports whether ref, a possibly short ID or a bare container name, names
// this container. Docker prefixes names with a slash.
func (e inspectEntry) is(ref string) bool {
	return ref != "" && (strings.HasPrefix(e.ID, ref) || strings.TrimPrefix(e.Name, "/") == ref)
}

// since mirrors the systemd runtime: a running container dates from its last
// start, a stopped one from its last stop. Docker's never-set time,
// 0001-01-01T00:00:00Z, parses as the zero time.
func (e inspectEntry) since() time.Time {
	if e.Running {
		return e.Started
	}
	return e.Finished
}

func parseInspect(raw []byte) ([]inspectEntry, error) {
	var out []inspectEntry
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e inspectEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("unparseable container inspect %q: %w", line, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// stateFromStatus recovers a state from the human status string on compose
// versions that omit the State field.
func stateFromStatus(status string) string {
	s := strings.ToLower(status)
	switch {
	case strings.HasPrefix(s, "up"):
		return "running"
	case strings.HasPrefix(s, "restarting"):
		return "restarting"
	case strings.HasPrefix(s, "exited"), strings.HasPrefix(s, "dead"):
		return "exited"
	case strings.HasPrefix(s, "created"):
		return "created"
	case strings.HasPrefix(s, "paused"):
		return "paused"
	}
	return "unknown"
}

// Logs streams the project's container logs.
func (r *Runtime) Logs(ctx context.Context, t *runtime.Target, opts runtime.LogOptions, w io.Writer) error {
	args := []string{"docker", "compose", "--project-name", ActiveProject(ctx, t), "logs", "--no-color"}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if opts.Tail > 0 {
		args = append(args, "--tail", fmt.Sprint(opts.Tail))
	}
	if opts.Since != "" {
		args = append(args, "--since", opts.Since)
	}

	code, err := t.Client.Stream(ctx, transport.Join(args...), w, io.Discard)
	if err != nil {
		return err
	}
	if code != 0 && ctx.Err() == nil {
		return fmt.Errorf("reading logs for %s: exit %d", t.Label(), code)
	}
	return nil
}

// Fingerprint digests the live stack: the config compose actually resolves
// (which folds in the release's compose file and .env) plus the image each
// container is really running.
//
// Hashing the resolved config rather than the file on disk means a hand-edited
// compose file, a changed .env, and a container silently running a different
// image all surface as drift.
func (r *Runtime) Fingerprint(ctx context.Context, t *runtime.Target) (string, error) {
	h := release.NewHasher()

	res, err := t.Client.Run(ctx, activeArgs(ctx, t, "config"))
	if err != nil {
		return "", err
	}
	if !res.OK() {
		return "", fmt.Errorf("resolving compose config for %s: %w", t.Label(), res.Err())
	}
	h.AddString("config", res.Stdout)

	res, err = t.Client.Run(ctx, transport.Join("docker", "compose",
		"--project-name", ActiveProject(ctx, t), "ps", "--all", "--format", "json"))
	if err != nil {
		return "", err
	}
	if res.OK() {
		instances, err := ParsePS([]byte(res.Stdout))
		if err == nil {
			images := map[string]string{}
			for _, in := range instances {
				images[in.Name] = in.Image
			}
			h.AddMap("images", images)
		}
	}

	return h.Sum(), nil
}
