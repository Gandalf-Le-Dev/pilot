package alert

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseBooleanMetrics(t *testing.T) {
	for _, expr := range []string{"service.down", "service.degraded", "deploy.failed", "drift.detected"} {
		c, err := Parse(expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", expr, err)
		}
		if c.Op != OpNone || c.Metric.Kind() != Boolean {
			t.Errorf("%q parsed as %+v", expr, c)
		}
	}
}

func TestParseNumericMetrics(t *testing.T) {
	tests := []struct {
		expr  string
		op    Op
		value float64
	}{
		{"service.restarts > 3", OpGT, 3},
		{"service.restarts >= 1", OpGTE, 1},
		{"host.disk.free_pct < 10", OpLT, 10},
		{"host.disk.used_pct <= 90", OpLTE, 90},
		{"host.disk.used_pct == 100", OpEQ, 100},
		{"service.restarts!=0", OpNEQ, 0},
		{"  host.disk.free_pct   <   15.5  ", OpLT, 15.5},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			c, err := Parse(tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			if c.Op != tc.op || c.Value != tc.value {
				t.Errorf("got op=%q value=%v", c.Op, c.Value)
			}
		})
	}
}

// `>=` must not be read as `>` followed by a stray `=`.
func TestParsePrefersLongerOperators(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want Op
	}{
		{"service.restarts >= 3", OpGTE},
		{"host.disk.free_pct <= 3", OpLTE},
		{"service.restarts != 3", OpNEQ},
		{"service.restarts == 3", OpEQ},
	} {
		c, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		if c.Op != tc.want {
			t.Errorf("Parse(%q) op = %q, want %q", tc.expr, c.Op, tc.want)
		}
	}
}

// Getting the two kinds confused is the likely mistake, so each error should
// say which kind the metric actually is.
func TestParseErrors(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{"", "empty"},
		{"service.explode", "unknown metric"},
		{"service.down > 1", "yes/no condition"},
		{"service.restarts", "is a number"},
		{"host.disk.free_pct < lots", "not a number"},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			_, err := Parse(tc.expr)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestUnknownMetricListsTheKnownOnes(t *testing.T) {
	_, err := Parse("service.explode")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "service.down") {
		t.Errorf("error should list what is available: %v", err)
	}
}

func TestScope(t *testing.T) {
	if ServiceDown.Scope() != ScopeService {
		t.Error("service.down should be per-service")
	}
	if DiskFreePct.Scope() != ScopeHost {
		t.Error("host.disk.free_pct should be host-wide")
	}
}

func TestEval(t *testing.T) {
	r := Reading{
		ServiceDown: true, Restarts: 5, DiskUsedPct: 88, DriftDetected: true,
	}
	tests := []struct {
		expr string
		want bool
	}{
		{"service.down", true},
		{"service.degraded", false},
		{"drift.detected", true},
		{"deploy.failed", false},
		{"service.restarts > 3", true},
		{"service.restarts > 10", false},
		{"host.disk.used_pct > 80", true},
		// free_pct is derived, so it must agree with used_pct.
		{"host.disk.free_pct < 15", true},
		{"host.disk.free_pct < 10", false},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			c, err := Parse(tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.Eval(r); got != tc.want {
				t.Errorf("Eval = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- engine ---

type capture struct {
	mu   sync.Mutex
	sent []Notification
	err  error
}

func (c *capture) Send(ctx context.Context, notifier string, msg Notification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg)
	return c.err
}

func (c *capture) all() []Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Notification(nil), c.sent...)
}

// clock is a controllable time source, so the state machine is tested by
// stepping time rather than by sleeping.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newEngine(t *testing.T) (*Engine, *capture, *clock) {
	t.Helper()
	cap := &capture{}
	clk := &clock{t: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)}

	e := NewEngine("web-1", cap)
	e.Now = clk.now
	return e, cap, clk
}

func rule(t *testing.T, subject, expr string, forDur time.Duration) Rule {
	t.Helper()
	c, err := Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	return Rule{Subject: subject, Cond: c, For: forDur, Notify: []string{"ntfy"}}
}

// A rule must hold for its full `for:` duration before it fires. This is the
// real anti-flap mechanism.
func TestRuleMustHoldForItsDuration(t *testing.T) {
	e, cap, clk := newEngine(t)
	r := rule(t, "api", "service.down", 2*time.Minute)
	down := map[string]Reading{"api": {ServiceDown: true}}
	ctx := context.Background()

	e.Evaluate(ctx, []Rule{r}, down)
	e.Flush()
	if n := len(cap.all()); n != 0 {
		t.Fatalf("fired immediately (%d sent); it should wait for `for:`", n)
	}

	clk.add(90 * time.Second)
	e.Evaluate(ctx, []Rule{r}, down)
	e.Flush()
	if n := len(cap.all()); n != 0 {
		t.Fatalf("fired at 90s of a 2m rule (%d sent)", n)
	}

	clk.add(45 * time.Second)
	e.Evaluate(ctx, []Rule{r}, down)
	e.Flush()

	sent := cap.all()
	if len(sent) != 1 {
		t.Fatalf("got %d notifications, want 1", len(sent))
	}
	if sent[0].Severity != SevFiring || sent[0].Service != "api" || sent[0].Host != "web-1" {
		t.Errorf("notification = %+v", sent[0])
	}
}

// A service bouncing every thirty seconds never stays down long enough to fire.
func TestFlappingServiceNeverFires(t *testing.T) {
	e, cap, clk := newEngine(t)
	r := rule(t, "api", "service.down", 2*time.Minute)
	ctx := context.Background()

	for range 20 {
		clk.add(30 * time.Second)
		e.Evaluate(ctx, []Rule{r}, map[string]Reading{"api": {ServiceDown: true}})
		clk.add(30 * time.Second)
		e.Evaluate(ctx, []Rule{r}, map[string]Reading{"api": {ServiceDown: false}})
	}
	e.Flush()

	if n := len(cap.all()); n != 0 {
		t.Errorf("a flapping service produced %d notifications; `for:` should have suppressed them all", n)
	}
}

// Something that stays broken all night must not send a message every fifteen
// seconds — that is how people learn to ignore alerts.
func TestCooldownSuppressesRepeats(t *testing.T) {
	e, cap, clk := newEngine(t)
	r := rule(t, "api", "service.down", 0)
	r.Cooldown = time.Hour
	down := map[string]Reading{"api": {ServiceDown: true}}
	ctx := context.Background()

	// Eight hours of evaluating every fifteen seconds.
	for range 8 * 60 * 4 {
		e.Evaluate(ctx, []Rule{r}, down)
		clk.add(15 * time.Second)
	}
	e.Flush()

	sent := cap.all()
	if len(sent) != 8 {
		t.Errorf("got %d notifications over 8 hours with a 1h cooldown, want 8", len(sent))
	}
}

func TestResolutionIsSentOnce(t *testing.T) {
	e, cap, clk := newEngine(t)
	r := rule(t, "api", "service.down", 0)
	ctx := context.Background()

	e.Evaluate(ctx, []Rule{r}, map[string]Reading{"api": {ServiceDown: true}})
	clk.add(time.Minute)
	e.Evaluate(ctx, []Rule{r}, map[string]Reading{"api": {ServiceDown: false}})
	clk.add(time.Minute)
	e.Evaluate(ctx, []Rule{r}, map[string]Reading{"api": {ServiceDown: false}})
	e.Flush()

	sent := cap.all()
	if len(sent) != 2 {
		t.Fatalf("got %d notifications, want a firing and one resolution", len(sent))
	}
	if sent[0].Severity != SevFiring || sent[1].Severity != SevResolved {
		t.Errorf("severities = %q, %q", sent[0].Severity, sent[1].Severity)
	}
	if !strings.Contains(sent[1].Summary, "recovered") {
		t.Errorf("resolution should read as recovery: %q", sent[1].Summary)
	}
}

// A missing observation is not evidence of health. Resolving on a gap would
// announce "recovered" for a host that had merely gone quiet.
func TestMissingReadingDoesNotResolve(t *testing.T) {
	e, cap, clk := newEngine(t)
	r := rule(t, "api", "service.down", 0)
	ctx := context.Background()

	e.Evaluate(ctx, []Rule{r}, map[string]Reading{"api": {ServiceDown: true}})
	clk.add(time.Minute)
	e.Evaluate(ctx, []Rule{r}, map[string]Reading{}) // no data this round
	e.Flush()

	sent := cap.all()
	if len(sent) != 1 {
		t.Fatalf("got %d notifications, want only the original firing", len(sent))
	}
	if len(e.Firing()) != 1 {
		t.Error("the rule should still be considered firing")
	}
}

func TestNotificationCarriesTheObservedValue(t *testing.T) {
	e, cap, _ := newEngine(t)
	r := rule(t, "", "host.disk.free_pct < 15", 0)

	e.Evaluate(context.Background(), []Rule{r}, map[string]Reading{"": {DiskUsedPct: 91}})
	e.Flush()

	sent := cap.all()
	if len(sent) != 1 {
		t.Fatalf("got %d notifications", len(sent))
	}
	if !strings.Contains(sent[0].Detail, "9") {
		t.Errorf("detail should report the actual figure, got %q", sent[0].Detail)
	}
	if sent[0].Service != "" {
		t.Errorf("a host rule should have no service: %q", sent[0].Service)
	}
}

// Reordering the config must not reset what is currently firing.
func TestStateSurvivesRuleReordering(t *testing.T) {
	e, cap, clk := newEngine(t)
	a := rule(t, "api", "service.down", 0)
	b := rule(t, "blog", "service.down", 0)
	readings := map[string]Reading{"api": {ServiceDown: true}, "blog": {ServiceDown: true}}
	ctx := context.Background()

	e.Evaluate(ctx, []Rule{a, b}, readings)
	clk.add(time.Minute)
	e.Evaluate(ctx, []Rule{b, a}, readings) // same rules, swapped
	e.Flush()

	if n := len(cap.all()); n != 2 {
		t.Errorf("got %d notifications, want one per rule — reordering re-fired them", n)
	}
}

func TestFiringAndForget(t *testing.T) {
	e, _, _ := newEngine(t)
	r := rule(t, "api", "service.down", 0)

	e.Evaluate(context.Background(), []Rule{r}, map[string]Reading{"api": {ServiceDown: true}})
	if got := e.Firing(); len(got) != 1 || !strings.Contains(got[0], "api") {
		t.Fatalf("Firing = %v", got)
	}

	e.Forget("api")
	if got := e.Firing(); len(got) != 0 {
		t.Errorf("Firing after Forget = %v", got)
	}
}

// A wedged notifier must not stall the loop and thereby suppress every other
// alert on the host.
func TestDeliveryFailuresAreReportedNotFatal(t *testing.T) {
	cap := &capture{err: context.DeadlineExceeded}
	e := NewEngine("web-1", cap)

	var mu sync.Mutex
	var failures int
	e.OnError = func(rule, notifier string, err error) {
		mu.Lock()
		failures++
		mu.Unlock()
	}

	r := rule(t, "api", "service.down", 0)
	e.Evaluate(context.Background(), []Rule{r}, map[string]Reading{"api": {ServiceDown: true}})
	e.Flush()

	mu.Lock()
	defer mu.Unlock()
	if failures != 1 {
		t.Errorf("got %d reported failures, want 1", failures)
	}
}

func TestRegistryRejectsUnknownNotifier(t *testing.T) {
	r := NewRegistry([]Notifier{{Name: "ntfy", Type: TypeNtfy, URL: "https://ntfy.sh/x"}})
	if err := r.Send(context.Background(), "nope", Notification{}); err == nil {
		t.Error("want an error for an unknown notifier")
	}
}

func TestNotificationFormatting(t *testing.T) {
	n := Notification{
		Severity: SevFiring, Host: "web-1", Service: "api",
		Rule: "service.down", Summary: "the service is not running",
		Since: time.Date(2026, 7, 27, 3, 0, 0, 0, time.UTC),
	}
	if !strings.HasPrefix(n.Title(), "ALERT: api on web-1") {
		t.Errorf("title = %q", n.Title())
	}
	if !strings.Contains(n.Text(), "service.down") || !strings.Contains(n.Text(), "since") {
		t.Errorf("text = %q", n.Text())
	}

	n.Severity = SevResolved
	if !strings.HasPrefix(n.Title(), "RESOLVED:") {
		t.Errorf("resolved title = %q", n.Title())
	}

	n.Service = ""
	if strings.Contains(n.Title(), " on ") {
		t.Errorf("a host-level alert should not name a service: %q", n.Title())
	}
}

// The alert that wakes somebody has to say what is wrong, not restate the rule
// they already wrote. `service.degraded` plus "the service is running but not
// fully healthy" was strictly less than `pilot status` showed — and for a
// scheduled job it was false as well, since a timer is never running.
func TestNotificationCarriesTheRuntimeExplanation(t *testing.T) {
	e, cap, clk := newEngine(t)
	r := rule(t, "backup", "service.degraded", 0)

	stale := map[string]Reading{"backup": {
		ServiceDegraded: true,
		Detail:          "last succeeded 60d ago, past the 48h freshness bound",
	}}

	e.Evaluate(context.Background(), []Rule{r}, stale)
	clk.add(time.Minute)
	e.Evaluate(context.Background(), []Rule{r}, stale)
	e.Flush()

	sent := cap.all()
	if len(sent) == 0 {
		t.Fatal("no notification was sent")
	}
	if !strings.Contains(sent[0].Detail, "past the 48h freshness bound") {
		t.Errorf("Detail = %q, want the runtime's own explanation", sent[0].Detail)
	}
}

// A numeric rule must keep its measurement. The value answers "how bad" and the
// explanation answers "what is wrong"; carrying only the second would have
// traded one missing fact for another.
func TestNotificationKeepsMeasuredValueAlongsideDetail(t *testing.T) {
	e, cap, clk := newEngine(t)
	r := rule(t, "api", "service.restarts > 3", 0)

	flapping := map[string]Reading{"api": {Restarts: 9, Detail: "container restarting"}}

	e.Evaluate(context.Background(), []Rule{r}, flapping)
	clk.add(time.Minute)
	e.Evaluate(context.Background(), []Rule{r}, flapping)
	e.Flush()

	sent := cap.all()
	if len(sent) == 0 {
		t.Fatal("no notification was sent")
	}
	for _, want := range []string{"currently 9", "container restarting"} {
		if !strings.Contains(sent[0].Detail, want) {
			t.Errorf("Detail = %q, want it to contain %q", sent[0].Detail, want)
		}
	}
}

// An episode opens when a rule fires, closes when it resolves, and a cooldown
// repeat mid-episode must not mint a second one — the dashboard's history
// panel counts episodes, not notifications.
func TestEventsRecordEpisodes(t *testing.T) {
	e, _, clk := newEngine(t)
	r := rule(t, "api", "service.down", 0)
	down := map[string]Reading{"api": {ServiceDown: true}}
	up := map[string]Reading{"api": {ServiceDown: false}}
	ctx := context.Background()

	e.Evaluate(ctx, []Rule{r}, down) // fires
	clk.add(2 * time.Hour)
	e.Evaluate(ctx, []Rule{r}, down) // cooldown repeat, same episode
	clk.add(10 * time.Minute)
	e.Evaluate(ctx, []Rule{r}, up) // resolves
	clk.add(1 * time.Minute)
	e.Evaluate(ctx, []Rule{r}, down) // a second episode
	e.Flush()

	evs := e.Events()
	if len(evs) != 2 {
		t.Fatalf("got %d episodes, want 2:\n%+v", len(evs), evs)
	}
	// Newest first.
	if !evs[0].ResolvedAt.IsZero() {
		t.Errorf("the second episode is still firing, but has ResolvedAt %v", evs[0].ResolvedAt)
	}
	if evs[1].ResolvedAt.IsZero() {
		t.Error("the first episode resolved, but ResolvedAt is zero")
	}
	if evs[1].Subject != "api" || evs[1].Rule != "service.down" {
		t.Errorf("episode = %+v", evs[1])
	}
}

// A notifier failure marks the episode, so the dashboard can say the operator
// was never actually told.
func TestEventsRecordDeliveryFailure(t *testing.T) {
	e, cap, _ := newEngine(t)
	cap.err = fmt.Errorf("webhook: 500")
	r := rule(t, "api", "service.down", 0)

	e.Evaluate(context.Background(), []Rule{r}, map[string]Reading{"api": {ServiceDown: true}})
	e.Flush()

	evs := e.Events()
	if len(evs) != 1 || !evs[0].DeliveryFailed {
		t.Fatalf("episode should be marked undelivered: %+v", evs)
	}
}
