package caddy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
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

	// LockWait is how long a route change waits for another Pilot's host
	// lock; zero means DefaultLockWait.
	LockWait time.Duration
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

	// Refuse to build on top of damage a previous run may have left.
	if err := checkNotTruncated(ctx, r, p, content); err != nil {
		return ActionNone, err
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

	// From here the file has been touched, so every failure path restores.
	//
	// The earlier version only restored when *validation* failed, treating a
	// failed write as "nothing happened". A write can fail after truncating,
	// and that left a wrecked Caddyfile with a backup nobody knew to use.
	fail := func(cause error) (Action, error) {
		if _, rErr := r.Run(ctx, transport.Join("cp", "-p", backup, p.Caddyfile)); rErr != nil {
			return ActionNone, fmt.Errorf("%w\n\nrestoring the backup also failed: %v\n"+
				"restore it by hand:  sudo cp -p %s %s", cause, rErr, backup, p.Caddyfile)
		}
		return ActionNone, fmt.Errorf("%w\n\n%s was restored from %s", cause, p.Caddyfile, backup)
	}

	if err := r.WriteFile(ctx, p.Caddyfile, []byte(updated), "0644"); err != nil {
		return fail(err)
	}

	// Read back what actually landed. A write can report failure after having
	// changed the file, or succeed having written something else — and the
	// only way to know which is to look.
	if got, err := r.ReadFile(ctx, p.Caddyfile); err != nil {
		return fail(fmt.Errorf("could not read %s back after writing it: %w", p.Caddyfile, err))
	} else if string(got) != updated {
		return fail(fmt.Errorf("%s does not match what was written (wrote %d bytes, read back %d)",
			p.Caddyfile, len(updated), len(got)))
	}

	// Make sure the snippet directory exists, or the glob import resolves to
	// nothing and validation fails for a confusing reason.
	if res, err := r.Run(ctx, transport.Join("mkdir", "-p", p.SnippetDir)); err != nil {
		return fail(err)
	} else if !res.OK() {
		return fail(fmt.Errorf("creating %s: %w", p.SnippetDir, res.Err()))
	}

	if err := Validate(ctx, r, p); err != nil {
		return fail(err)
	}

	if err := Reload(ctx, r, p); err != nil {
		return fail(err)
	}
	return ActionInstalled, nil
}

// ShrinkRatio is how much smaller than its newest backup a Caddyfile may be
// before Pilot treats it as damaged rather than edited.
//
// Deliberately generous: deleting a site or two is normal, losing most of the
// file is not.
const ShrinkRatio = 0.5

// checkNotTruncated refuses to modify a Caddyfile that looks like the wreckage
// of an earlier failed run.
//
// This is the second half of the lesson. Restoring on failure prevents the
// damage; this prevents a *retry* from compounding it — which is what actually
// took the sites down, when a second `--fix` appended an import line to a file
// that had already lost sixty-eight lines, then reloaded Caddy with it.
func checkNotTruncated(ctx context.Context, r Runner, p Paths, content string) error {
	res, err := r.Run(ctx, "ls -1t "+transport.Quote(p.Caddyfile)+".pilot-bak-* 2>/dev/null | head -1")
	if err != nil || !res.OK() || res.Out() == "" {
		return nil // no previous run to compare against
	}
	newest := res.Out()

	prev, err := r.ReadFile(ctx, newest)
	if err != nil {
		return nil // unreadable backup is not evidence of anything
	}

	if len(prev) == 0 || float64(len(content)) >= float64(len(prev))*ShrinkRatio {
		return nil
	}

	return fmt.Errorf("%s is %d bytes but the backup %s is %d — it looks like a previous run damaged it\n"+
		"refusing to modify it further; compare them and restore if needed:\n"+
		"    sudo diff %s %s\n"+
		"    sudo cp -p %s %s",
		p.Caddyfile, len(content), newest, len(prev),
		newest, p.Caddyfile, newest, p.Caddyfile)
}

// Timing of the host lock on route changes. A lock older than LockStale
// belongs to a Pilot that died holding it.
const (
	DefaultLockWait = 30 * time.Second
	LockStale       = 10 * time.Minute
)

// lockDir is the host lock, a directory because mkdir is atomic everywhere.
// It holds the holder's token, so a release only ever removes its own lock.
const lockDir = ".pilot-lock"

// errLockHeld is the exit status of the lock script when it gave up waiting.
const errLockHeld = 75

// LockHeld reports whether a Pilot holds the host lock on route files.
func LockHeld(ctx context.Context, r Runner, p Paths) (bool, error) {
	res, err := r.Run(ctx, transport.Join("test", "-d", path.Join(p.SnippetDir, lockDir)))
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

// lockRoutes takes the host-wide lock on Pilot's route files and returns its
// release.
//
// Services that share a domain write the same site block, and the last one
// to leave removes it. The agent, a CLI deploying without one, `pilot routes
// --prune` and `pilot doctor --fix` can all change routes on one host at
// once, and so can the agent's own jobs for different services. Each list,
// write, validate and reload must run alone.
//
// A stale lock is renamed away before it is removed, and only when the
// renamed directory still holds the token seen when judging it stale: two
// waiters that judged it stale together cannot both break it, nor can one
// break the fresh lock the other just took.
func lockRoutes(ctx context.Context, r Runner, p Paths) (func(), error) {
	wait := p.LockWait
	if wait <= 0 {
		wait = DefaultLockWait
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(tok)
	lock := path.Join(p.SnippetDir, lockDir)
	l, owner := transport.Quote(lock), transport.Quote(path.Join(lock, "owner"))
	moved := transport.Quote(lock+".stale.") + "$$"

	res, err := r.Run(ctx, fmt.Sprintf("mkdir -p %s || exit 1; i=0; "+
		"while ! mkdir %s 2>/dev/null; do "+
		"old=$(cat %s 2>/dev/null); "+
		"if [ -n \"$(find %s -maxdepth 0 -mmin +%d 2>/dev/null)\" ] && mv %s %s 2>/dev/null; then "+
		"if [ \"$(cat %s/owner 2>/dev/null)\" = \"$old\" ]; then rm -rf %s; else mv %s %s 2>/dev/null; fi; continue; fi; "+
		"i=$((i+1)); [ \"$i\" -ge %d ] && exit %d; sleep 1; done; "+
		"printf '%%s\\n' %s > %s || { rm -rf %s; exit 1; }",
		transport.Quote(p.SnippetDir), l, owner, l, int(LockStale.Minutes()), l, moved,
		moved, moved, moved, l,
		int(wait.Seconds()), errLockHeld,
		token, owner, l))
	switch {
	case err != nil:
		return nil, err
	case res.ExitCode == errLockHeld:
		return nil, fmt.Errorf("another Pilot is changing the routes on this host: %s is still held after waiting %s\n"+
			"retry when it finishes; if no Pilot is running there, remove that directory", lock, wait)
	case !res.OK():
		return nil, fmt.Errorf("locking %s: %w", lock, res.Err())
	}
	return func() {
		_, _ = r.Run(context.WithoutCancel(ctx), fmt.Sprintf("[ \"$(cat %s 2>/dev/null)\" = %s ] && rm -rf %s; true", owner, token, l))
	}, nil
}

// InstallSnippet installs a service's route, reloading Caddy only when a file
// actually changed.
//
// The comparison is why most deploys never touch Caddy at all: a release
// that changes only application code renders a byte-identical route.
//
// A route can change layout, from a site block of its own to handles in
// shared site blocks or back, so the files it leaves go in the same validated
// reload as the files it takes. Removals land first, so that no state on disk
// in between holds two blocks for one domain.
func InstallSnippet(ctx context.Context, r Runner, p Paths, service, content string) (Action, error) {
	unlock, err := lockRoutes(ctx, r, p)
	if err != nil {
		return ActionNone, err
	}
	defer unlock()

	installed, err := listRoutes(ctx, r, p)
	if err != nil {
		return ActionNone, err
	}
	want := routeFiles(service, content)

	var changes []fileChange
	owned := false
	for _, rel := range staleFiles(installed, service, want, routeDomains(content)) {
		target := path.Join(p.SnippetDir, rel)
		prev, err := r.ReadFile(ctx, target)
		if err != nil {
			return ActionNone, fmt.Errorf("reading %s: %w", target, err)
		}
		if !strings.HasPrefix(rel, sitePrefix) || strings.Contains(rel, "/") {
			owned = true
		}
		changes = append(changes, fileChange{path: target, remove: true, prev: prev, existed: true})
	}
	dirs := []string{p.SnippetDir}
	for _, f := range want {
		target := path.Join(p.SnippetDir, f.rel)
		if dir := path.Dir(target); !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
		prev, err := r.ReadFile(ctx, target)
		if err == nil && !f.site {
			owned = true
		}
		if err == nil && string(prev) == f.content {
			continue
		}
		changes = append(changes, fileChange{path: target, content: f.content, prev: prev, existed: err == nil})
	}
	if len(changes) == 0 {
		return ActionNone, nil
	}
	if err := checkSites(ctx, r, p, installed, service, content); err != nil {
		return ActionNone, err
	}
	action := ActionUpdated
	if !owned {
		action = ActionInstalled
	}

	// A route with no bind of its own joins the catch-all :443 server. If the
	// host's Caddyfile binds sites to explicit addresses, that server never
	// sees public traffic — yet the route installs, validates, gets a
	// certificate, and serves an empty 200 to every request, so nothing
	// downstream would ever flag it. Refusing here is the only reliable
	// moment: this is where the operator's Caddyfile is in reach.
	if !routeBinds(content) {
		if raw, err := r.ReadFile(ctx, p.Caddyfile); err == nil {
			if siteBind, defaultBind := BindUsage(string(raw)); siteBind && !defaultBind {
				return ActionNone, fmt.Errorf(
					"%s binds sites to explicit addresses, so a route without `bind` would sit in a server public traffic never reaches — installed, TLS-valid, and serving an empty 200\n"+
						"declare the host's addresses as hosts.<host>.caddy.bind in fleet.yaml, or add `default_bind` to the Caddyfile's global options",
					p.Caddyfile)
			}
		}
	}

	if res, err := r.Run(ctx, transport.Join(append([]string{"mkdir", "-p"}, dirs...)...)); err != nil {
		return ActionNone, err
	} else if !res.OK() {
		return ActionNone, fmt.Errorf("creating %s: %w", strings.Join(dirs, ", "), res.Err())
	}

	if err := applyChanges(ctx, r, p, changes); err != nil {
		var u *undoneError
		switch {
		case !errors.As(err, &u):
			return ActionNone, err
		case len(u.lost) > 0:
			return ActionNone, fmt.Errorf("route for %s is invalid: %w\n\n"+
				"the previous route could NOT be restored — %s left in an unknown state\n"+
				"fix it by hand before Caddy next restarts", service, u.cause, strings.Join(u.lost, ", "))
		case action == ActionInstalled:
			return ActionNone, fmt.Errorf("route for %s is invalid: %w", service, u.cause)
		}
		return ActionNone, fmt.Errorf("route for %s is invalid: %w\nthe previous route was restored", service, u.cause)
	}
	return action, nil
}

// fileChange is one file a route change writes or removes, with what it held
// before, so that a failed change can put it back.
type fileChange struct {
	path    string
	content string
	remove  bool
	prev    []byte
	existed bool
}

// undoneError is a route change that was refused and put back. lost names
// the files that could not be restored.
type undoneError struct {
	cause error
	lost  []string
}

func (e *undoneError) Error() string { return e.cause.Error() }

// applyChanges makes a route change, validates it, and reloads.
//
// It undoes rather than leave a config Caddy will reject on its next restart,
// which would turn a bad deploy into an outage hours later. A failed write
// gets the same treatment as a failed validation, because a write that
// reports failure may still have changed the file. A failed reload is
// returned as it is: Caddy keeps serving the previous config.
func applyChanges(ctx context.Context, r Runner, p Paths, changes []fileChange) error {
	var applied []fileChange
	undo := func(cause error) error {
		u := &undoneError{cause: cause}
		for i := len(applied) - 1; i >= 0; i-- {
			c := applied[i]
			if !c.existed {
				_, _ = r.Run(ctx, transport.Join("rm", "-f", c.path))
				continue
			}
			// Restoring uses the same write that just failed, so confirm it
			// landed rather than assume. A route left as garbage would be
			// found by Caddy on its next restart, hours later and far from
			// the cause.
			_ = r.WriteFile(ctx, c.path, c.prev, "0644")
			if got, err := r.ReadFile(ctx, c.path); err != nil || string(got) != string(c.prev) {
				u.lost = append(u.lost, c.path)
			}
		}
		return u
	}

	for _, c := range changes {
		applied = append(applied, c)
		if c.remove {
			if res, err := r.Run(ctx, transport.Join("rm", "-f", c.path)); err != nil {
				return undo(err)
			} else if !res.OK() {
				return undo(fmt.Errorf("removing %s: %w", c.path, res.Err()))
			}
			continue
		}
		if err := r.WriteFile(ctx, c.path, []byte(c.content), "0644"); err != nil {
			return undo(err)
		}
		if got, err := r.ReadFile(ctx, c.path); err != nil || string(got) != c.content {
			return undo(fmt.Errorf("%s does not match what was written", c.path))
		}
	}

	if err := Validate(ctx, r, p); err != nil {
		return undo(explainConflict(err))
	}
	if err := Reload(ctx, r, p); err != nil {
		return err
	}
	removeEmptySiteDirs(ctx, r, changes)
	return nil
}

// explainConflict adds the remedy to the one rejection a shared domain can
// cause: its shared site block next to a block that serves the whole domain.
func explainConflict(err error) error {
	if !strings.Contains(err.Error(), "ambiguous site definition") {
		return err
	}
	return fmt.Errorf("%w\n\n"+
		"a domain that several services share is served only by Pilot's %s<domain>.caddy\n"+
		"redeploy the service whose route still serves the whole domain so it joins that site, "+
		"remove routes of services no longer placed here (`pilot routes --prune`), "+
		"or move a block you wrote for the domain into a `handle`", err, sitePrefix)
}

// removeEmptySiteDirs tidies the handle directory of each shared site block
// that went. It is cosmetic, since Caddy only reads a directory through its
// site block's import.
func removeEmptySiteDirs(ctx context.Context, r Runner, changes []fileChange) {
	for _, c := range changes {
		if c.remove && strings.HasPrefix(path.Base(c.path), sitePrefix) {
			_, _ = r.Run(ctx, transport.Join("rmdir", strings.TrimSuffix(c.path, ".caddy")))
		}
	}
}

// RemoveSnippet deletes a service's route and reloads, with each shared site
// block it leaves empty.
func RemoveSnippet(ctx context.Context, r Runner, p Paths, service string) (Action, error) {
	unlock, err := lockRoutes(ctx, r, p)
	if err != nil {
		return ActionNone, err
	}
	defer unlock()

	installed, err := listRoutes(ctx, r, p)
	if err != nil {
		return ActionNone, err
	}
	stale := staleFiles(installed, service, nil, nil)
	if len(stale) == 0 {
		return ActionNone, nil
	}

	var changes []fileChange
	for _, rel := range stale {
		target := path.Join(p.SnippetDir, rel)
		if res, err := r.Run(ctx, transport.Join("rm", "-f", target)); err != nil {
			return ActionNone, err
		} else if !res.OK() {
			return ActionNone, fmt.Errorf("removing route for %s: %w", service, res.Err())
		}
		changes = append(changes, fileChange{path: target, remove: true})
	}

	if err := Reload(ctx, r, p); err != nil {
		return ActionNone, err
	}
	removeEmptySiteDirs(ctx, r, changes)
	return ActionRemoved, nil
}

// SiteProblems lists what a failed or hand-made change can leave of the shared
// sites, relative to the snippet dir: site blocks with no handles to import,
// which answer their domain with an empty 200 and block a route that wants
// the domain to itself, and handle directories no site block imports, whose
// routes Caddy never serves.
func SiteProblems(ctx context.Context, r Runner, p Paths) (empty []string, headless []HeadlessSite, err error) {
	installed, err := listRoutes(ctx, r, p)
	if err != nil {
		return nil, nil, err
	}
	empty, headless = siteProblems(installed)
	return empty, headless, nil
}

// TidySites removes each shared site block with no handles and writes the
// missing site block of each handle directory, from the handles' own header,
// in one validated reload. It returns the files it changed.
func TidySites(ctx context.Context, r Runner, p Paths) ([]string, error) {
	unlock, err := lockRoutes(ctx, r, p)
	if err != nil {
		return nil, err
	}
	defer unlock()

	installed, err := listRoutes(ctx, r, p)
	if err != nil {
		return nil, err
	}
	empty, headless := siteProblems(installed)

	var changes []fileChange
	var changed []string
	for _, rel := range empty {
		target := path.Join(p.SnippetDir, rel)
		prev, err := r.ReadFile(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", target, err)
		}
		changes = append(changes, fileChange{path: target, remove: true, prev: prev, existed: true})
		changed = append(changed, rel)
	}
	for _, h := range headless {
		dir := h.Dir
		site, err := siteFor(ctx, r, p, installed, dir)
		if err != nil {
			return nil, err
		}
		changes = append(changes, fileChange{path: path.Join(p.SnippetDir, dir+".caddy"), content: site})
		changed = append(changed, dir+".caddy")
	}
	if len(changes) == 0 {
		return nil, nil
	}

	if err := applyChanges(ctx, r, p, changes); err != nil {
		var u *undoneError
		if errors.As(err, &u) && len(u.lost) > 0 {
			return nil, fmt.Errorf("%w\n\nthese could NOT be restored, fix them by hand before Caddy next restarts: %s",
				u.cause, strings.Join(u.lost, ", "))
		}
		return nil, err
	}
	return changed, nil
}

// siteFor renders the missing site block of a handle directory from the
// header of the handles in it.
func siteFor(ctx context.Context, r Runner, p Paths, installed []string, dir string) (string, error) {
	for _, rel := range installed {
		if !strings.HasPrefix(rel, dir+"/") {
			continue
		}
		b, err := r.ReadFile(ctx, path.Join(p.SnippetDir, rel))
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", rel, err)
		}
		if sr, ok := parseShared(string(b)); ok {
			for _, d := range sr.domains {
				if siteName(d) == dir {
					return renderSite(d, sr.bind), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no route in %s/ names its domain, so its site block cannot be rebuilt; redeploy the services in it", path.Join(p.SnippetDir, dir))
}

// listRoutes returns the route files on the host, relative to the snippet
// dir: the top-level *.caddy files and the handles under each shared site.
//
// A directory that cannot be read is an error, not an empty list: the list
// decides which old files a change removes and which site blocks go, and a
// glob in an unreadable directory silently matches nothing. A missing snippet
// dir holds no routes.
func listRoutes(ctx context.Context, r Runner, p Paths) ([]string, error) {
	dir := transport.Quote(p.SnippetDir)
	res, err := r.Run(ctx, "[ -d "+dir+" ] || exit 0; cd "+dir+" && [ -r . ] || exit 2; "+
		"for d in "+sitePrefix+"*/; do [ ! -d \"$d\" ] || [ -r \"$d\" ] || exit 2; done; "+
		"for f in *.caddy "+sitePrefix+"*/*.caddy; do [ ! -e \"$f\" ] || printf '%s\\n' \"$f\"; done")
	switch {
	case err != nil:
		return nil, err
	case !res.OK():
		return nil, fmt.Errorf("listing the routes in %s: %w", p.SnippetDir, res.Err())
	}
	var out []string
	for line := range strings.SplitSeq(res.Out(), "\n") {
		if name := strings.TrimSpace(line); strings.HasSuffix(name, ".caddy") {
			out = append(out, name)
		}
	}
	return out, nil
}

// ListSnippets returns the service names Pilot currently has routes for,
// whether as a site block of their own or as handles in a shared site.
func ListSnippets(ctx context.Context, r Runner, p Paths) ([]string, error) {
	files, err := listRoutes(ctx, r, p)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rel := range files {
		dir, file := path.Split(rel)
		svc := strings.TrimSuffix(file, ".caddy")
		if svc == "" || (dir == "" && strings.HasPrefix(file, sitePrefix)) || slices.Contains(out, svc) {
			continue
		}
		out = append(out, svc)
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
