package caddy

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/ssh"
)

// Runner is the subset of an ssh client these operations need. Narrowing it
// keeps Caddy knowledge in one place while staying testable with a fake.
type Runner interface {
	Run(ctx context.Context, cmd string) (ssh.Result, error)
	RunScript(ctx context.Context, body string) (ssh.Result, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, mode string) error
}

// Paths locates Caddy's files on a host.
type Paths struct {
	Caddyfile  string
	SnippetDir string
	Admin      string
}

// Action reports what an idempotent operation actually did, so the CLI can say
// "already configured" instead of implying it changed something.
type Action string

const (
	ActionNone      Action = "unchanged"
	ActionInstalled Action = "installed"
	ActionUpdated   Action = "updated"
	ActionRemoved   Action = "removed"
)

// Validate asks Caddy whether the current config parses. It is the cheap check
// that turns a bad route into a staging failure rather than an outage.
func Validate(ctx context.Context, r Runner, p Paths) error {
	res, err := r.Run(ctx, transport.Join("caddy", "validate", "--config", p.Caddyfile))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("caddy rejected the configuration:\n%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Reload applies the configuration.
//
// Caddy keeps serving the previous config if the new one fails to load, so a
// failure here is a failed activation rather than downtime.
func Reload(ctx context.Context, r Runner, p Paths) error {
	res, err := r.Run(ctx, transport.Join("caddy", "reload", "--config", p.Caddyfile))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("caddy reload failed (previous config still serving):\n%s",
			strings.TrimSpace(res.Stderr))
	}
	return nil
}

// AdminReachable reports whether Caddy's admin API is responding.
func AdminReachable(ctx context.Context, r Runner, p Paths) (bool, error) {
	if p.Admin == "" {
		return false, nil
	}
	res, err := r.Run(ctx, transport.Join("curl", "-fsS", "--max-time", "5", strings.TrimSuffix(p.Admin, "/")+"/config/"))
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

// BackupName is where EnsureImport copies the Caddyfile before touching it.
func BackupName(caddyfile string, at time.Time) string {
	return fmt.Sprintf("%s.pilot-bak-%s", caddyfile, at.UTC().Format("20060102T150405Z"))
}

// EnsureImport adds Pilot's import line to the global Caddyfile.
//
// This is the single edit Pilot makes outside the directory it owns, so it is
// deliberately careful: it is idempotent, it refuses the one file shape where
// appending would silently do the wrong thing, it keeps a timestamped backup,
// and it restores that backup if Caddy then rejects the result.
func EnsureImport(ctx context.Context, r Runner, p Paths, at time.Time) (Action, error) {
	raw, err := r.ReadFile(ctx, p.Caddyfile)
	if err != nil {
		return ActionNone, fmt.Errorf("reading %s: %w", p.Caddyfile, err)
	}
	content := string(raw)

	switch state, reason := Inspect(content, p.Caddyfile, p.SnippetDir); state {
	case ImportPresent:
		return ActionNone, nil
	case ImportUnsafe:
		return ActionNone, fmt.Errorf("refusing to edit %s: %s\n%s", p.Caddyfile, reason, UnsafeHint)
	}

	updated, err := AppendImport(content, p.Caddyfile, p.SnippetDir)
	if err != nil {
		return ActionNone, err
	}

	backup := BackupName(p.Caddyfile, at)
	if res, err := r.Run(ctx, transport.Join("cp", "-p", p.Caddyfile, backup)); err != nil {
		return ActionNone, err
	} else if !res.OK() {
		return ActionNone, fmt.Errorf("backing up %s: %w", p.Caddyfile, res.Err())
	}

	if err := r.WriteFile(ctx, p.Caddyfile, []byte(updated), "0644"); err != nil {
		return ActionNone, err
	}

	// Make sure the snippet directory exists, or the glob import resolves to
	// nothing and validation fails for a confusing reason.
	if res, err := r.Run(ctx, transport.Join("mkdir", "-p", p.SnippetDir)); err != nil {
		return ActionNone, err
	} else if !res.OK() {
		return ActionNone, fmt.Errorf("creating %s: %w", p.SnippetDir, res.Err())
	}

	if err := Validate(ctx, r, p); err != nil {
		if _, rErr := r.Run(ctx, transport.Join("cp", "-p", backup, p.Caddyfile)); rErr != nil {
			return ActionNone, fmt.Errorf("%w\n\nrestoring the backup also failed: %v\nrestore it by hand from %s", err, rErr, backup)
		}
		return ActionNone, fmt.Errorf("%w\n\n%s was restored from %s", err, p.Caddyfile, backup)
	}

	if err := Reload(ctx, r, p); err != nil {
		return ActionNone, err
	}
	return ActionInstalled, nil
}

// InstallSnippet writes a service's route, reloading Caddy only when the file
// actually changed.
//
// The hash comparison is why most deploys never touch Caddy at all: a release
// that changes only application code renders a byte-identical snippet.
func InstallSnippet(ctx context.Context, r Runner, p Paths, service, content string) (Action, error) {
	target := path.Join(p.SnippetDir, SnippetName(service))

	existing, err := r.ReadFile(ctx, target)
	if err == nil && string(existing) == content {
		return ActionNone, nil
	}
	action := ActionUpdated
	if err != nil {
		action = ActionInstalled
	}

	if res, err := r.Run(ctx, transport.Join("mkdir", "-p", p.SnippetDir)); err != nil {
		return ActionNone, err
	} else if !res.OK() {
		return ActionNone, fmt.Errorf("creating %s: %w", p.SnippetDir, res.Err())
	}

	if err := r.WriteFile(ctx, target, []byte(content), "0644"); err != nil {
		return ActionNone, err
	}

	if err := Validate(ctx, r, p); err != nil {
		// Undo rather than leave a config Caddy will reject on its next
		// restart — which would turn a bad deploy into an outage later.
		if action == ActionInstalled {
			_, _ = r.Run(ctx, transport.Join("rm", "-f", target))
		} else {
			_ = r.WriteFile(ctx, target, existing, "0644")
		}
		return ActionNone, fmt.Errorf("route for %s is invalid: %w", service, err)
	}

	if err := Reload(ctx, r, p); err != nil {
		return ActionNone, err
	}
	return action, nil
}

// RemoveSnippet deletes a service's route and reloads.
func RemoveSnippet(ctx context.Context, r Runner, p Paths, service string) (Action, error) {
	target := path.Join(p.SnippetDir, SnippetName(service))

	res, err := r.Run(ctx, transport.Join("test", "-e", target))
	if err != nil {
		return ActionNone, err
	}
	if !res.OK() {
		return ActionNone, nil
	}

	if res, err := r.Run(ctx, transport.Join("rm", "-f", target)); err != nil {
		return ActionNone, err
	} else if !res.OK() {
		return ActionNone, fmt.Errorf("removing route for %s: %w", service, res.Err())
	}

	if err := Reload(ctx, r, p); err != nil {
		return ActionNone, err
	}
	return ActionRemoved, nil
}

// ListSnippets returns the service names Pilot currently has routes for.
func ListSnippets(ctx context.Context, r Runner, p Paths) ([]string, error) {
	res, err := r.Run(ctx, "ls -1 "+transport.Quote(p.SnippetDir)+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(res.Out(), "\n") {
		name := strings.TrimSpace(line)
		if svc, ok := strings.CutSuffix(name, ".caddy"); ok && svc != "" {
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Orphans returns routes on the host with no corresponding service in the
// fleet — usually left behind when a service was removed from config.
func Orphans(installed []string, known map[string]bool) []string {
	var out []string
	for _, svc := range installed {
		if !known[svc] {
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out
}
