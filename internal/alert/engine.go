package alert

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultCooldown is how long a firing rule stays quiet before repeating
// itself. Without it, a service that stays down all night sends a message every
// evaluation, and the person on the receiving end learns to ignore them.
const DefaultCooldown = 1 * time.Hour

// Rule is one configured alert.
type Rule struct {
	// Subject is the service the rule concerns, empty for host-wide rules.
	Subject string

	Cond     Condition
	For      time.Duration
	Cooldown time.Duration
	Notify   []string
}

// Key identifies a rule for state tracking. Rules are keyed by subject and
// expression rather than by position, so reordering the config does not reset
// what is currently firing.
func (r Rule) Key() string { return r.Subject + "\x00" + r.Cond.Raw }

func (r Rule) cooldown() time.Duration {
	if r.Cooldown > 0 {
		return r.Cooldown
	}
	return DefaultCooldown
}

// state tracks one rule's progress toward firing.
type state struct {
	pendingSince time.Time
	firing       bool
	lastNotified time.Time
}

// Sender delivers a notification. The engine takes an interface so tests can
// observe delivery without a network.
type Sender interface {
	Send(ctx context.Context, notifier string, msg Notification) error
}

// Engine evaluates rules and decides what to send.
//
// The evaluation is pure and lives entirely on the host. That is the point:
// alerting has to work when there is no central server, no dashboard, and
// nobody's laptop is open.
type Engine struct {
	Host   string
	Sender Sender

	// Now is overridable in tests.
	Now func() time.Time

	// OnError reports a delivery failure. Delivery problems are logged rather
	// than retried: a queue that survives restarts is a bigger thing than this
	// should be, and silently dropping is worse than saying so.
	OnError func(rule string, notifier string, err error)

	mu   sync.Mutex
	seen map[string]*state

	workerOnce sync.Once
	queue      chan delivery
	pending    sync.WaitGroup
}

// NewEngine returns an engine.
func NewEngine(host string, sender Sender) *Engine {
	return &Engine{
		Host:   host,
		Sender: sender,
		Now:    func() time.Time { return time.Now().UTC() },
		seen:   map[string]*state{},
	}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

// Evaluate runs every rule against the readings for its subject and sends what
// it must. Readings is keyed by subject, with "" holding the host-wide reading.
func (e *Engine) Evaluate(ctx context.Context, rules []Rule, readings map[string]Reading) {
	for _, r := range rules {
		reading, ok := readings[r.Subject]
		if !ok {
			// No observation for this subject this round. Absence of data is
			// not evidence of health, so leave any existing state alone rather
			// than resolving on a gap.
			continue
		}
		if msg, notifiers := e.step(r, reading); msg != nil {
			e.dispatch(ctx, r, *msg, notifiers)
		}
	}
}

// step advances one rule's state machine and returns a notification if one is
// due.
//
// The `for:` duration is the real anti-flap mechanism: a service bouncing every
// thirty seconds never stays down long enough to fire. The cooldown handles the
// other case — something that stays broken for hours.
func (e *Engine) step(r Rule, reading Reading) (*Notification, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	key := r.Key()

	st, ok := e.seen[key]
	if !ok {
		st = &state{}
		e.seen[key] = st
	}

	holds := r.Cond.Eval(reading)

	if !holds {
		if st.firing {
			st.firing = false
			st.pendingSince = time.Time{}
			return e.build(r, reading, SevResolved, time.Time{}), r.Notify
		}
		st.pendingSince = time.Time{}
		return nil, nil
	}

	if st.pendingSince.IsZero() {
		st.pendingSince = now
	}
	if now.Sub(st.pendingSince) < r.For {
		return nil, nil // true, but not for long enough yet
	}

	if st.firing && now.Sub(st.lastNotified) < r.cooldown() {
		return nil, nil // already reported, still within the quiet period
	}

	since := st.pendingSince
	st.firing = true
	st.lastNotified = now
	return e.build(r, reading, SevFiring, since), r.Notify
}

func (e *Engine) build(r Rule, reading Reading, sev Severity, since time.Time) *Notification {
	msg := &Notification{
		Severity: sev,
		Host:     e.Host,
		Service:  r.Subject,
		Rule:     r.Cond.Raw,
		Summary:  r.Cond.Describe(),
		Since:    since,
		At:       e.now(),
	}
	// Both facts, when both exist. The measured value answers "how bad", the
	// runtime's own line answers "what is wrong" — and the second is what was
	// missing: a rule name plus "the service is running but not fully healthy"
	// told an operator less than the dashboard they were not looking at, and
	// for a scheduled job it was not even true, since a timer is never running.
	var detail []string
	if v, ok := r.Cond.Value_(reading); ok {
		detail = append(detail, fmt.Sprintf("currently %s", trimFloat(v)))
	}
	if reading.Detail != "" {
		detail = append(detail, reading.Detail)
	}
	msg.Detail = strings.Join(detail, " — ")
	if sev == SevResolved {
		msg.Summary = "recovered: " + msg.Summary
	}
	return msg
}

// delivery is one queued message.
type delivery struct {
	rule     string
	notifier string
	msg      Notification
}

// QueueDepth bounds the outbound queue. Deep enough to absorb a burst, shallow
// enough that a permanently broken notifier cannot consume memory without
// bound.
const QueueDepth = 256

// dispatch hands messages to the delivery worker.
//
// Sending through one ordered queue rather than a goroutine per message is
// deliberate: a "resolved" arriving before the "firing" it resolves is worse
// than useless, and that is exactly what concurrent delivery produces when two
// evaluations land close together. The queue keeps order while still leaving
// the evaluation loop free of a slow webhook.
func (e *Engine) dispatch(ctx context.Context, r Rule, msg Notification, notifiers []string) {
	if e.Sender == nil {
		return
	}
	e.startWorker()

	for _, name := range notifiers {
		e.pending.Add(1)
		select {
		case e.queue <- delivery{rule: r.Cond.Raw, notifier: name, msg: msg}:
		default:
			// Full. Dropping and saying so beats blocking the evaluation loop
			// and thereby suppressing every other alert on the host.
			e.pending.Done()
			if e.OnError != nil {
				e.OnError(r.Cond.Raw, name, errQueueFull)
			}
		}
	}
}

var errQueueFull = fmt.Errorf("alert queue is full; message dropped")

func (e *Engine) startWorker() {
	e.workerOnce.Do(func() {
		e.queue = make(chan delivery, QueueDepth)
		go func() {
			for d := range e.queue {
				err := e.Sender.Send(context.Background(), d.notifier, d.msg)
				if err != nil && e.OnError != nil {
					e.OnError(d.rule, d.notifier, err)
				}
				e.pending.Done()
			}
		}()
	})
}

// Notify sends a one-off message to every named notifier.
//
// Separate from Evaluate because an event is not a condition: there is no
// firing state to enter, nothing to resolve later, and no cooldown to suppress
// a repeat. Two deploys a minute apart are two facts, and collapsing them would
// hide the second one.
//
// It reuses the same ordered delivery queue as alerts, so a deploy notification
// cannot overtake a firing alert that was decided first.
func (e *Engine) Notify(ctx context.Context, msg Notification, notifiers []string) {
	e.dispatch(ctx, Rule{}, msg, notifiers)
}

// Flush waits for every queued message to be delivered. Used by tests and on
// shutdown, so a stopping daemon does not swallow an alert it had already
// decided to send.
func (e *Engine) Flush() { e.pending.Wait() }

// Firing lists the rules currently in the firing state, sorted, for `pilot
// status` and for the agent's own reporting.
func (e *Engine) Firing() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []string
	for key, st := range e.seen {
		if st.firing {
			subject, expr, _ := cut(key)
			if subject != "" {
				out = append(out, subject+": "+expr)
			} else {
				out = append(out, expr)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Forget drops a subject's state, so a removed service stops being tracked.
func (e *Engine) Forget(subject string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for key := range e.seen {
		if s, _, _ := cut(key); s == subject {
			delete(e.seen, key)
		}
	}
}

func cut(key string) (subject, expr string, ok bool) {
	for i := range len(key) {
		if key[i] == 0 {
			return key[:i], key[i+1:], true
		}
	}
	return "", key, false
}

// Registry maps notifier names to their configuration.
type Registry struct {
	byName map[string]Notifier
}

// NewRegistry indexes notifiers by name.
func NewRegistry(ns []Notifier) *Registry {
	m := map[string]Notifier{}
	for _, n := range ns {
		m[n.Name] = n
	}
	return &Registry{byName: m}
}

// Send delivers through the named notifier.
func (r *Registry) Send(ctx context.Context, name string, msg Notification) error {
	n, ok := r.byName[name]
	if !ok {
		return fmt.Errorf("no notifier named %q", name)
	}
	return n.Send(ctx, msg)
}
