// Package systemd implements the Runtime for services supervised by systemd.
//
// Pilot adopts a unit that already exists rather than writing one — config.Unit
// explains why. What this adapter owns is the release: it stages a directory of
// files, validates them before anything is live, points `current` at them,
// relinks whatever paths the unit's ExecStart depends on, and restarts the unit
// behind a health gate.
//
// Two shapes of unit are supported and they differ in every method. A `service`
// is a daemon: it is up or it is not. A `oneshot` runs and exits, so there is
// nothing to observe as "up" — its health is whether the last run succeeded and
// how long ago that was. Modelling only the first would have produced an
// adapter that could not express a backup job, which is exactly the sort of
// thing that gets called generic right up until the second consumer arrives.
package systemd

import (
	"context"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

type Runtime struct{}

func New() *Runtime { return &Runtime{} }

func (r *Runtime) Kind() config.Runtime { return config.RuntimeSystemd }

// unitOf returns the service's unit block. Validation rejects a systemd service
// without one, so reaching this error means a caller bypassed validation rather
// than that a user wrote bad config.
func unitOf(t *runtime.Target) (*config.Unit, error) {
	u := t.Service.Unit
	if u == nil || u.Name == "" {
		return nil, fmt.Errorf("service %q has no `unit.name`", t.Service.Name)
	}
	return u, nil
}

// Stage ships the release and then proves it before anything swaps.
func (r *Runtime) Stage(ctx context.Context, t *runtime.Target, in runtime.StageInput) error {
	if _, err := unitOf(t); err != nil {
		return err
	}
	if err := runtime.StageCommon(ctx, t, in); err != nil {
		return err
	}
	return r.precheck(ctx, t)
}

// precheck runs the service's own validation against the staged release.
//
// It runs with the release directory as its working directory, so `./hopboxd
// --check` names the incoming binary rather than the live one. No templating
// and no substitution: the working directory carries the meaning, which is one
// fewer thing to get subtly wrong.
//
// This is the step that separates "the deploy failed and nothing changed" from
// "the deploy failed and the daemon is down". A config error caught here costs
// nothing; the same error caught after the swap costs an outage and a rollback.
func (r *Runtime) precheck(ctx context.Context, t *runtime.Target) error {
	u := t.Service.Unit
	if len(u.Precheck) == 0 {
		return nil
	}

	cmd := fmt.Sprintf("cd %s && %s", transport.Quote(t.ReleaseDir()), transport.Join(u.Precheck...))
	res, err := t.Client.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("precheck failed for %s, so nothing was activated and %s is untouched: %s",
			t.Label(), u.Name, detailOf(res))
	}
	return nil
}

// Activate points the host at the new release and restarts the unit.
func (r *Runtime) Activate(ctx context.Context, t *runtime.Target) error {
	u, err := unitOf(t)
	if err != nil {
		return err
	}

	// Links are ensured before the swap, not after. They resolve through
	// `current`, so at this point they still name the outgoing release — which
	// is what is already live — and the swap then moves the link targets and
	// the release together. That keeps the symlink swap as the single commit
	// point, the same one every other runtime uses.
	if err := r.ensureLinks(ctx, t); err != nil {
		return err
	}
	if err := runtime.Swap(ctx, t); err != nil {
		return err
	}

	target := u.RestartTarget()
	if target == "" {
		// A oneshot with no timer. The swap is the whole deploy: whatever
		// triggers the job next will pick up the new release. Restarting the
		// service unit here would run the job immediately, which a deploy did
		// not ask for and which for a backup could mean hours of work.
		return nil
	}

	res, err := t.Client.Run(ctx, transport.Join("systemctl", "restart", target))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("restarting %s on %s: %s", target, t.Host.Name, detailOf(res))
	}
	return nil
}

// ensureLinks points the configured host paths into the live release.
//
// Idempotent, so running it on every activate costs nothing and repairs a link
// somebody removed.
func (r *Runtime) ensureLinks(ctx context.Context, t *runtime.Target) error {
	u := t.Service.Unit
	if len(u.Links) == 0 {
		return nil
	}

	res, err := t.Client.RunScript(ctx, linkScript(u, t.CurrentDir()))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("linking %s: %s", t.Label(), detailOf(res))
	}
	return nil
}

// linkScript builds the shell that repoints the configured host paths.
//
// Every link target goes through `current`, never a release directory. That is
// what makes rollback free: the symlink swap alone changes what every link
// resolves to, so there is no second set of pointers to keep in step and no way
// for the binary and the release to disagree.
func linkScript(u *config.Unit, currentDir string) string {
	var lines []string
	for _, dst := range u.LinkPaths() {
		src := path.Join(currentDir, u.Links[dst])
		tmp := dst + ".pilot-tmp"

		lines = append(lines,
			// Refuse to replace a real file. If /usr/local/bin/x is a binary
			// somebody installed by hand, quietly converting it to a symlink
			// is how two tools end up fighting over one path, with the loser
			// being whichever ran last.
			fmt.Sprintf("if [ -e %s ] && [ ! -L %s ]; then", transport.Quote(dst), transport.Quote(dst)),
			// Name the remedy. Refusing is right, but this fires on the most
			// ordinary path there is — adopting a service whose binary someone
			// installed by hand — and "refusing to replace it" with no next
			// step reads as a wall rather than a decision to make.
			fmt.Sprintf("  echo %s >&2", transport.Quote(dst+" exists and is not a symlink, so Pilot will not replace it")),
			fmt.Sprintf("  echo %s >&2", transport.Quote(
				"    if Pilot should own this path, move the existing file aside first: sudo mv "+dst+" "+dst+".pre-pilot")),
			"  exit 1",
			"fi",
			fmt.Sprintf("mkdir -p %s", transport.Quote(path.Dir(dst))),
			// Same two-step as the release swap: create beside, then rename
			// over. `ln -sfn` alone is not atomic and leaves a window where
			// the path does not exist.
			fmt.Sprintf("ln -sfn %s %s", transport.Quote(src), transport.Quote(tmp)),
			fmt.Sprintf("mv -Tf %s %s", transport.Quote(tmp), transport.Quote(dst)),
		)
	}

	return strings.Join(lines, "\n")
}

// Deactivate stops the unit, and its timer first when there is one — otherwise
// stopping a oneshot would be undone by the next scheduled trigger.
func (r *Runtime) Deactivate(ctx context.Context, t *runtime.Target) error {
	u, err := unitOf(t)
	if err != nil {
		return err
	}

	units := []string{}
	if u.Timer != "" {
		units = append(units, u.Timer)
	}
	units = append(units, u.Name)

	res, err := t.Client.Run(ctx, transport.Join(append([]string{"systemctl", "stop"}, units...)...))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("stopping %s on %s: %s", u.Name, t.Host.Name, detailOf(res))
	}
	return nil
}

// showProps are the properties Observe reads. One request covers both unit
// kinds: systemd returns an empty value for a property a unit does not have
// rather than failing, so there is no need to know the shape before asking.
var showProps = []string{
	"ActiveState", "SubState", "Result", "NRestarts",
	"ActiveEnterTimestamp", "ExecMainStartTimestamp", "ExecMainExitTimestamp",
	"ExecMainStatus", "LoadState",
}

// Observe reports what systemd says about the unit.
func (r *Runtime) Observe(ctx context.Context, t *runtime.Target) (runtime.Observation, error) {
	obs := runtime.Observation{State: runtime.StateUnknown}

	u, err := unitOf(t)
	if err != nil {
		return obs, err
	}

	current, err := runtime.ReadCurrent(ctx, t)
	if err != nil {
		return obs, err
	}
	obs.Release = current

	props, err := r.show(ctx, t, u.Name)
	if err != nil {
		return obs, err
	}

	// A unit systemd has never heard of is not a stopped service. Reporting it
	// as stopped would send an operator looking for a crash instead of for the
	// unit file that was never installed.
	if props["LoadState"] == "not-found" || len(props) == 0 {
		obs.Detail = fmt.Sprintf("systemd has no unit %s on this host", u.Name)
		return obs, nil
	}

	if u.IsOneshot() {
		return r.observeOneshot(ctx, t, u, props, obs)
	}
	return observeService(u, props, obs)
}

// observeService maps a daemon's ActiveState onto Pilot's vocabulary.
func observeService(u *config.Unit, props map[string]string, obs runtime.Observation) (runtime.Observation, error) {
	active, sub := props["ActiveState"], props["SubState"]

	inst := runtime.Instance{Name: u.Name, State: sub}
	if n, err := strconv.Atoi(props["NRestarts"]); err == nil {
		inst.Restarts = n
	}
	if ts := parseStamp(props["ActiveEnterTimestamp"]); !ts.IsZero() {
		obs.Since, inst.Since = ts, ts
	}

	switch active {
	case "active":
		obs.State = runtime.StateRunning
	case "activating", "reloading", "deactivating":
		// In transition. Not yet a failure and not yet fine — a health gate
		// polling this will resolve it one way or the other shortly.
		obs.State = runtime.StateDegraded
		obs.Detail = active
	case "failed":
		obs.State = runtime.StateFailed
		obs.Detail = fmt.Sprintf("unit failed (result=%s)", props["Result"])
	case "inactive":
		obs.State = runtime.StateStopped
	default:
		obs.Detail = fmt.Sprintf("unexpected ActiveState %q", active)
	}

	obs.Instances = []runtime.Instance{inst}
	return obs, nil
}

// observeOneshot decides whether a scheduled job is actually working.
//
// The trap here is Result. systemd initialises it to "success", so a unit that
// has never run once reports exactly the same Result as one that ran and
// succeeded. Trusting it would mean a backup job reporting healthy forever
// without a single run having happened — the precise failure this runtime
// exists to surface, reproduced by the tool meant to catch it.
//
// ExecMainStartTimestamp is the honest signal: empty means the unit has never
// been started, whatever Result claims.
func (r *Runtime) observeOneshot(ctx context.Context, t *runtime.Target, u *config.Unit,
	props map[string]string, obs runtime.Observation,
) (runtime.Observation, error) {
	verdict := classifyOneshot(u, props, time.Now().UTC())

	obs.State, obs.Detail, obs.Since = verdict.state, verdict.detail, verdict.last
	inst := runtime.Instance{Name: u.Name, State: verdict.instance, Since: verdict.last}

	// The next scheduled run is decoration on an otherwise bare line, so it is
	// fetched last and a failure to read it is ignored rather than allowed to
	// fail the whole observation.
	if verdict.wantNext {
		if next := r.nextRun(ctx, t, u); next != "" {
			obs.Detail = strings.TrimPrefix(obs.Detail+", next "+next, ", ")
		}
	}

	obs.Instances = []runtime.Instance{inst}
	return obs, nil
}

// oneshotVerdict is the pure conclusion about a scheduled job.
type oneshotVerdict struct {
	state    runtime.State
	detail   string
	instance string
	last     time.Time

	// wantNext asks the caller to append the timer's next firing, which needs
	// a second round trip and so cannot happen in here.
	wantNext bool
}

// classifyOneshot decides whether a scheduled job is actually working.
//
// The trap this exists to avoid is Result. systemd initialises it to "success",
// so a unit that has never run once reports exactly the same Result as one that
// ran and succeeded — verified on a real host against a freshly installed timer
// that had never fired. Trusting it would mean a backup reporting healthy
// forever without a single run having happened, which is precisely the failure
// this runtime is supposed to surface, reproduced by the tool meant to catch it.
//
// ExecMainStartTimestamp is the honest signal: empty means the unit has never
// been started, whatever Result claims.
func classifyOneshot(u *config.Unit, props map[string]string, now time.Time) oneshotVerdict {
	if strings.TrimSpace(props["ExecMainStartTimestamp"]) == "" {
		return oneshotVerdict{
			state:    runtime.StateStopped,
			detail:   fmt.Sprintf("%s has never run", u.Name),
			instance: "never run",
			wantNext: true,
		}
	}

	// Mid-run. Not a verdict yet.
	if props["ActiveState"] == "activating" {
		return oneshotVerdict{
			state:    runtime.StateRunning,
			detail:   "running now",
			instance: props["SubState"],
		}
	}

	if props["Result"] != "success" {
		return oneshotVerdict{
			state: runtime.StateFailed,
			detail: fmt.Sprintf("last run failed (result=%s, exit=%s)",
				props["Result"], props["ExecMainStatus"]),
			instance: props["SubState"],
		}
	}

	last := parseStamp(props["ExecMainExitTimestamp"])
	v := oneshotVerdict{
		state:    runtime.StateRunning,
		instance: props["SubState"],
		last:     last,
		wantNext: true,
	}

	// A job that succeeded, but two months ago, is not healthy. With no
	// freshness bound there is nothing to compare against and the last success
	// stands forever — which is why validation warns when `fresh` is unset.
	if fresh := u.Fresh.Duration(); fresh > 0 && !last.IsZero() {
		if age := now.Sub(last); age > fresh {
			v.state = runtime.StateDegraded
			v.detail = fmt.Sprintf("last succeeded %s ago, past the %s freshness bound",
				humanDuration(age), humanDuration(fresh))
			v.wantNext = false
		}
	}
	return v
}

// humanDuration renders a span the way an operator would say it.
//
// time.Duration's own String gives "1440h0m0s" for two months, which is
// technically correct and unreadable — and this string lands in `pilot status`,
// next to the one line explaining why a backup is degraded. Being legible at a
// glance is the whole job of that line.
// The switch to days starts at 72h rather than 48h so that a `fresh: 48h` from
// the config is echoed back as "48h" and not silently restated as "2d".
// Freshness bounds are written in hours by convention, and a message that
// converts an operator's own value into different units reads like it is
// talking about something else.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 72*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// nextRun reports when the timer fires next, best-effort. It is decoration on
// a detail line, so a failure to read it must not fail the observation.
func (r *Runtime) nextRun(ctx context.Context, t *runtime.Target, u *config.Unit) string {
	if u.Timer == "" {
		return ""
	}
	props, err := r.show(ctx, t, u.Timer)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(props["NextElapseUSecRealtime"])
}

// show reads unit properties as a map.
func (r *Runtime) show(ctx context.Context, t *runtime.Target, unit string) (map[string]string, error) {
	args := append([]string{"systemctl", "show", unit, "-p"},
		strings.Join(append(showProps, "NextElapseUSecRealtime"), ","))

	res, err := t.Client.Run(ctx, transport.Join(args...))
	if err != nil {
		return nil, err
	}
	return parseShow(res.Stdout), nil
}

// parseShow turns `systemctl show` output into a map.
//
// Split on the first `=` only: values routinely contain them — a timestamp
// does not, but ExecStart and Environment do, and a parser that split on every
// `=` would silently truncate them.
func parseShow(out string) map[string]string {
	props := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		props[k] = v
	}
	return props
}

// stampLayout is systemd's default human-readable timestamp, as emitted by
// `systemctl show` — e.g. "Thu 2026-07-30 09:13:59 UTC".
const stampLayout = "Mon 2006-01-02 15:04:05 MST"

// parseStamp reads a systemd timestamp, returning the zero time for the empty
// string systemd uses to mean "never".
func parseStamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	ts, err := time.Parse(stampLayout, s)
	if err != nil {
		return time.Time{}
	}
	return ts.UTC()
}

// Logs streams the unit's journal.
func (r *Runtime) Logs(ctx context.Context, t *runtime.Target, opts runtime.LogOptions, w io.Writer) error {
	u, err := unitOf(t)
	if err != nil {
		return err
	}

	args := []string{"journalctl", "--unit", u.Name, "--no-pager"}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if opts.Tail > 0 {
		args = append(args, "--lines", strconv.Itoa(opts.Tail))
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

// Fingerprint digests the unit definition and the link targets, not only the
// release.
//
// Pilot does not own the unit file, so detection is the compensating control:
// an operator who edits the unit by hand, or relinks a binary out from under
// `current`, changes this digest and shows up as drift. `systemctl cat`
// includes drop-in overrides, which is where such an edit usually lives —
// reading the unit file alone would miss a /etc/systemd/system/x.service.d/
// override entirely.
func (r *Runtime) Fingerprint(ctx context.Context, t *runtime.Target) (string, error) {
	u, err := unitOf(t)
	if err != nil {
		return "", err
	}

	lines := []string{transport.Join("systemctl", "cat", u.Name)}
	for _, dst := range u.LinkPaths() {
		// `readlink -f` would resolve through `current` to the release
		// directory, so every deploy would look like drift. The link's own
		// target is the stable thing: it names `current` and only changes when
		// somebody repoints it.
		lines = append(lines, fmt.Sprintf("echo %s; readlink %s || echo MISSING",
			transport.Quote(dst), transport.Quote(dst)))
	}

	res, err := t.Client.RunScript(ctx, strings.Join(lines, "\n"))
	if err != nil {
		return "", err
	}
	if !res.OK() {
		return "", fmt.Errorf("fingerprinting %s: %s", t.Label(), detailOf(res))
	}

	h := release.NewHasher()
	h.AddString("unit", res.Stdout)
	return h.Sum(), nil
}

// detailOf picks the most informative one-line explanation from a result.
// systemctl reports its problems on stderr, but a failing precheck usually
// prints to stdout, so preferring one over the other loses half the cases.
func detailOf(res transport.Result) string {
	if s := firstLine(res.Stderr); s != "" {
		return s
	}
	if s := firstLine(res.Stdout); s != "" {
		return s
	}
	return fmt.Sprintf("exit %d", res.ExitCode)
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return head
}
