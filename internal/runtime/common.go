package runtime

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/gandalfledev/pilot/internal/release"
	"github.com/gandalfledev/pilot/internal/transport"
)

// StageCommon performs the part of staging that is identical for every runtime:
// create the release directory, ship its contents, and write the files Pilot
// itself owns.
//
// Ordering matters. The bulk upload runs first because rsync stages with
// --delete; writing .env before it would see the secret file deleted as an
// unexpected extra.
func StageCommon(ctx context.Context, t *Target, in StageInput) error {
	relDir := t.ReleaseDir()

	if err := t.Client.MkdirAll(ctx, relDir); err != nil {
		return fmt.Errorf("creating release directory on %s: %w", t.Host.Name, err)
	}

	if in.LocalDir != "" {
		if err := t.Client.UploadDir(ctx, in.LocalDir, relDir); err != nil {
			return fmt.Errorf("staging %s: %w", t.Label(), err)
		}
	}

	if len(in.Env) > 0 {
		body, err := RenderEnv(in.Env)
		if err != nil {
			return fmt.Errorf("rendering environment for %s: %w", t.Service.Name, err)
		}
		// 0600: this file holds resolved secrets.
		if err := t.Client.WriteFile(ctx, path.Join(relDir, release.EnvFile), body, "0600"); err != nil {
			return fmt.Errorf("writing environment for %s: %w", t.Label(), err)
		}
	}

	if in.Manifest != nil {
		body, err := release.MarshalManifest(in.Manifest)
		if err != nil {
			return err
		}
		if err := t.Client.WriteFile(ctx, path.Join(relDir, release.ManifestFile), body, "0644"); err != nil {
			return fmt.Errorf("writing manifest for %s: %w", t.Label(), err)
		}
	}

	if in.Snippet != "" {
		if err := t.Client.WriteFile(ctx, path.Join(relDir, release.SnippetFile), []byte(in.Snippet), "0644"); err != nil {
			return fmt.Errorf("writing route for %s: %w", t.Label(), err)
		}
	}

	return nil
}

// SwapScript returns the shell that atomically activates t.Release.
//
// `mv -T` is not optional. Without it, moving a symlink onto an existing
// symlink-to-a-directory moves it *inside* that directory, leaving `current`
// pointing at the old release with a stray link nested underneath.
func SwapScript(t *Target) string {
	svcDir := t.ServiceDir()
	current := path.Join(svcDir, release.CurrentLink)
	pending := path.Join(svcDir, release.PendingLink)
	target := release.CurrentTarget(t.Release)

	return strings.Join([]string{
		fmt.Sprintf("test -d %s", transport.Quote(t.ReleaseDir())),
		fmt.Sprintf("if [ -e %s ] && [ ! -L %s ]; then", transport.Quote(current), transport.Quote(current)),
		fmt.Sprintf("  echo %s >&2", transport.Quote(current+" exists but is not a symlink; refusing to replace it")),
		"  exit 1",
		"fi",
		fmt.Sprintf("ln -sfn %s %s", transport.Quote(target), transport.Quote(pending)),
		fmt.Sprintf("mv -Tf %s %s", transport.Quote(pending), transport.Quote(current)),
	}, "\n")
}

// Swap activates t.Release on the host.
//
// An executor that can do this in-process does — that is the agent, running on
// the machine itself. Only a remote executor needs the shell form.
func Swap(ctx context.Context, t *Target) error {
	if !release.IsID(t.Release) {
		return fmt.Errorf("malformed release id %q", t.Release)
	}

	if s, ok := t.Client.(transport.Swapper); ok {
		if err := s.Swap(t.ServiceDir(), t.Release); err != nil {
			return fmt.Errorf("activating %s: %w", t.Label(), err)
		}
		return nil
	}

	res, err := t.Client.RunScript(ctx, SwapScript(t))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("activating %s: %w", t.Label(), res.Err())
	}
	return nil
}

// ReadCurrent returns the live release ID on the host, or "" if none.
func ReadCurrent(ctx context.Context, t *Target) (string, error) {
	res, err := t.Client.Run(ctx, "readlink "+transport.Quote(t.CurrentDir()))
	if err != nil {
		return "", err
	}
	if !res.OK() {
		return "", nil // not deployed yet
	}
	return path.Base(res.Out()), nil
}

// ListReleases returns the release IDs staged on the host, oldest first.
func ListReleases(ctx context.Context, t *Target) ([]string, error) {
	dir := t.Layout.Releases(t.Service.Name)
	res, err := t.Client.Run(ctx, "ls -1 "+transport.Quote(dir)+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var ids []string
	for line := range strings.SplitSeq(res.Out(), "\n") {
		name := strings.TrimSpace(line)
		if release.IsID(name) {
			ids = append(ids, name)
		}
	}
	return release.SortIDs(ids), nil
}

// GC removes superseded releases on the host, keeping the newest `keep` plus
// anything named in protect.
func GC(ctx context.Context, t *Target, keep int, protect ...string) ([]string, error) {
	ids, err := ListReleases(ctx, t)
	if err != nil {
		return nil, err
	}
	remove := release.SelectForGC(ids, keep, protect...)
	for _, id := range remove {
		if err := t.Client.RemoveAll(ctx, t.Layout.Release(t.Service.Name, id)); err != nil {
			return nil, fmt.Errorf("pruning release %s on %s: %w", id, t.Host.Name, err)
		}
	}
	return remove, nil
}

// ReadState loads the service's state.json from the host, returning a fresh
// state when the file is absent.
func ReadState(ctx context.Context, t *Target) (*release.State, error) {
	res, err := t.Client.Run(ctx, "cat "+transport.Quote(t.Layout.State(t.Service.Name))+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return release.NewState(t.Service.Name), nil
	}
	return release.UnmarshalState([]byte(res.Stdout))
}

// WriteState persists the service's state.json on the host.
func WriteState(ctx context.Context, t *Target, s *release.State) error {
	body, err := release.MarshalState(s)
	if err != nil {
		return err
	}
	return t.Client.WriteFile(ctx, t.Layout.State(t.Service.Name), body, "0644")
}

// Reconcile brings the host's recorded state in line with what the symlink
// actually says.
//
// The filesystem wins. If state.json was lost, or someone swapped `current` by
// hand, the live release is whatever the link points at — believing the file
// over the filesystem is how a tool ends up confidently wrong.
func Reconcile(ctx context.Context, t *Target, s *release.State) (bool, error) {
	live, err := ReadCurrent(ctx, t)
	if err != nil {
		return false, err
	}
	if live == "" || live == s.Current {
		return false, nil
	}
	s.Promote(live)
	return true, nil
}

// ReadManifest loads one release's manifest from the host.
func ReadManifest(ctx context.Context, t *Target, id string) (*release.Manifest, error) {
	body, err := t.Client.ReadFile(ctx, t.Layout.Manifest(t.Service.Name, id))
	if err != nil {
		return nil, err
	}
	return release.UnmarshalManifest(body)
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RenderEnv formats resolved environment variables as a .env file.
//
// Keys are sorted so the file is byte-stable when nothing changed. Values that
// need it are double-quoted, a form both `docker compose --env-file` and
// systemd's EnvironmentFile understand. Newlines are rejected rather than
// escaped: neither consumer parses them consistently, so a silently mangled
// secret is the worst possible outcome.
func RenderEnv(env map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		if !envKeyPattern.MatchString(k) {
			return nil, fmt.Errorf("invalid environment variable name %q", k)
		}
		v := env[k]
		if strings.ContainsAny(v, "\n\r\x00") {
			return nil, fmt.Errorf("value of %s contains a newline, which .env cannot represent", k)
		}
		if needsQuoting(v) {
			v = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return []byte(b.String()), nil
}

func needsQuoting(v string) bool {
	if v == "" {
		return false
	}
	return strings.ContainsAny(v, " \t\"'#$`\\") ||
		strings.HasPrefix(v, " ") || strings.HasSuffix(v, " ")
}
