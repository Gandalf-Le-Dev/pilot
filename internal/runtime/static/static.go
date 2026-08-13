// Package static implements the Runtime for sites served straight from disk by
// Caddy.
//
// This is the runtime where the symlink does all the work: Caddy's root points
// at `current` and it stats that path per request, so activation is the swap
// and nothing else. There is no process to restart and no downtime at all.
package static

import (
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

type Runtime struct{}

func New() *Runtime { return &Runtime{} }

func (r *Runtime) Kind() config.Runtime { return config.RuntimeStatic }

// Stage uploads the built site and carries forward the previous release's
// hashed assets.
func (r *Runtime) Stage(ctx context.Context, t *runtime.Target, in runtime.StageInput) error {
	if err := runtime.StageCommon(ctx, t, in); err != nil {
		return err
	}
	return r.overlayPreviousAssets(ctx, t)
}

// overlayPreviousAssets hardlinks the outgoing release's asset directories into
// the incoming one, for files the new release doesn't have.
//
// Without this, a page loaded seconds before a swap can request a hash-named
// asset that exists only in the old release, and get a 404. Hardlinks make the
// carry-forward free in disk terms.
//
// It is scoped to named directories rather than the whole tree on purpose: a
// blanket overlay would mean deleted pages never actually disappear.
func (r *Runtime) overlayPreviousAssets(ctx context.Context, t *runtime.Target) error {
	dirs := overlayDirs(t.Service)
	if len(dirs) == 0 {
		return nil
	}

	previous, err := runtime.ReadCurrent(ctx, t)
	if err != nil || previous == "" || previous == t.Release {
		return err
	}
	prevDir := t.Layout.Release(t.Service.Name, previous)

	var lines []string
	for _, d := range dirs {
		src := path.Join(prevDir, d)
		dst := path.Join(t.ReleaseDir(), d)
		lines = append(lines,
			fmt.Sprintf("if [ -d %s ]; then", transport.Quote(src)),
			fmt.Sprintf("  mkdir -p %s", transport.Quote(dst)),
			// -n never clobbers the new release; -l hardlinks so this costs
			// no space; the fallback copy covers filesystems without links.
			fmt.Sprintf("  cp -aln %s/. %s/ 2>/dev/null || cp -an %s/. %s/ 2>/dev/null || true",
				transport.Quote(src), transport.Quote(dst), transport.Quote(src), transport.Quote(dst)),
			"fi",
		)
	}

	res, err := t.Client.RunScript(ctx, strings.Join(lines, "\n"))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("carrying forward assets for %s: %w", t.Label(), res.Err())
	}
	return nil
}

func overlayDirs(s *config.Service) []string {
	if s.Expose != nil && s.Expose.Static != nil {
		return s.Expose.Static.OverlayDirs()
	}
	return config.DefaultOverlayDirs
}

// Activate swaps the symlink. Caddy resolves it per request, so the new build
// is live immediately with no reload and no dropped connections.
func (r *Runtime) Activate(ctx context.Context, t *runtime.Target) error {
	return runtime.Swap(ctx, t)
}

// Deactivate is a no-op: a static site has no process. Taking it offline means
// removing its route, which the deploy pipeline owns.
func (r *Runtime) Deactivate(ctx context.Context, t *runtime.Target) error { return nil }

// Observe checks that a release is activated and that its entry point exists.
func (r *Runtime) Observe(ctx context.Context, t *runtime.Target) (runtime.Observation, error) {
	obs := runtime.Observation{State: runtime.StateUnknown}

	current, err := runtime.ReadCurrent(ctx, t)
	if err != nil {
		return obs, err
	}
	obs.Release = current
	if current == "" {
		obs.State = runtime.StateStopped
		obs.Detail = "no release is activated"
		return obs, nil
	}

	root := t.CurrentDir()
	index := indexFile(t.Service)

	script := strings.Join([]string{
		fmt.Sprintf("test -e %s && echo yes || echo no", transport.Quote(path.Join(root, index))),
		fmt.Sprintf("find -L %s -type f 2>/dev/null | wc -l", transport.Quote(root)),
	}, "\n")

	res, err := t.Client.RunScript(ctx, script)
	if err != nil {
		return obs, err
	}
	if !res.OK() {
		obs.Detail = strings.TrimSpace(res.Stderr)
		return obs, nil
	}

	fields := strings.Fields(res.Out())
	hasIndex := len(fields) > 0 && fields[0] == "yes"
	files := 0
	if len(fields) > 1 {
		files, _ = strconv.Atoi(fields[1])
	}

	inst := runtime.Instance{
		Name:   t.Service.Name,
		State:  "served",
		Detail: fmt.Sprintf("%d files", files),
	}

	switch {
	case !hasIndex:
		obs.State = runtime.StateDegraded
		obs.Detail = fmt.Sprintf("%s is missing from the live release", index)
		inst.State = "incomplete"
	case files == 0:
		obs.State = runtime.StateDegraded
		obs.Detail = "the live release is empty"
		inst.State = "empty"
	default:
		obs.State = runtime.StateRunning
	}

	obs.Instances = []runtime.Instance{inst}
	return obs, nil
}

func indexFile(s *config.Service) string {
	if s.Expose != nil && s.Expose.Static != nil && s.Expose.Static.Index != "" {
		return s.Expose.Static.Index
	}
	return config.DefaultIndex
}

// Logs reports that there is nothing to stream. Requests to a static site are
// recorded by Caddy, not per service, so pretending otherwise would be worse
// than saying so.
func (r *Runtime) Logs(ctx context.Context, t *runtime.Target, opts runtime.LogOptions, w io.Writer) error {
	return fmt.Errorf("%w: %s is served directly by Caddy — see Caddy's access log",
		runtime.ErrNoLogs, t.Service.Name)
}

// Fingerprint digests the live tree by relative path and size.
//
// Names and sizes rather than contents: hashing every byte of a site on every
// drift check would be slow enough that operators would turn it off, and any
// hand-edit large enough to matter changes a size or a filename.
//
// The agent caches the baseline as a file inside this very tree, so the
// measurement excludes it by name — the first check after a deploy would
// otherwise create the file it is about to be compared against, and every
// static service would report drift forever, starting one check-interval
// after activating.
func (r *Runtime) Fingerprint(ctx context.Context, t *runtime.Target) (string, error) {
	root := t.CurrentDir()
	script := fmt.Sprintf("find -L %s -type f -not -name %s -printf '%%P %%s\\n' 2>/dev/null | LC_ALL=C sort",
		transport.Quote(root), transport.Quote(release.FingerprintFile))

	res, err := t.Client.RunScript(ctx, script)
	if err != nil {
		return "", err
	}
	if !res.OK() {
		return "", fmt.Errorf("fingerprinting %s: %w", t.Label(), res.Err())
	}

	h := release.NewHasher()
	h.AddString("tree", res.Stdout)
	return h.Sum(), nil
}
