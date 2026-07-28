package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Severity distinguishes a problem from its resolution.
type Severity string

const (
	SevFiring   Severity = "firing"
	SevResolved Severity = "resolved"
)

// Notification is one message about one rule.
type Notification struct {
	Severity Severity  `json:"severity"`
	Host     string    `json:"host"`
	Service  string    `json:"service,omitempty"`
	Rule     string    `json:"rule"`
	Summary  string    `json:"summary"`
	Detail   string    `json:"detail,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	At       time.Time `json:"at"`
}

// Title is the one-line subject.
func (n Notification) Title() string {
	subject := n.Host
	if n.Service != "" {
		subject = n.Service + " on " + n.Host
	}
	if n.Severity == SevResolved {
		return "RESOLVED: " + subject
	}
	return "ALERT: " + subject
}

// Text is the message body, in the form a phone notification should read.
func (n Notification) Text() string {
	var b strings.Builder
	b.WriteString(n.Summary)
	if n.Detail != "" {
		b.WriteString("\n")
		b.WriteString(n.Detail)
	}
	if n.Severity == SevFiring && !n.Since.IsZero() {
		fmt.Fprintf(&b, "\nsince %s", n.Since.Format(time.RFC3339))
	}
	b.WriteString("\nrule: " + n.Rule)
	return b.String()
}

// NotifierType selects the wire format.
type NotifierType string

const (
	TypeWebhook NotifierType = "webhook"
	TypeNtfy    NotifierType = "ntfy"
	TypeSlack   NotifierType = "slack"
	TypeDiscord NotifierType = "discord"
	TypeCommand NotifierType = "command"
)

// AllNotifierTypes lists every supported notifier, for error messages.
var AllNotifierTypes = []string{
	string(TypeWebhook), string(TypeNtfy), string(TypeSlack),
	string(TypeDiscord), string(TypeCommand),
}

// Notifier is one delivery target.
//
// Each is a single HTTP POST or a single exec. There is no retry queue and no
// templating: a notifier that needs either should be a `command` pointing at
// something that does it properly.
type Notifier struct {
	Name    string
	Type    NotifierType
	URL     string
	Command []string
}

// deliveryTimeout bounds one send. A wedged webhook must not stall the alert
// loop and thereby suppress every other alert on the host.
const deliveryTimeout = 10 * time.Second

// Send delivers a notification.
func (n Notifier) Send(ctx context.Context, msg Notification) error {
	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()

	switch n.Type {
	case TypeWebhook:
		return n.postJSON(ctx, msg)
	case TypeNtfy:
		return n.postNtfy(ctx, msg)
	case TypeSlack:
		return n.postJSON(ctx, map[string]string{"text": msg.Title() + "\n" + msg.Text()})
	case TypeDiscord:
		return n.postJSON(ctx, map[string]string{"content": msg.Title() + "\n" + msg.Text()})
	case TypeCommand:
		return n.runCommand(ctx, msg)
	}
	return fmt.Errorf("notifier %q has unknown type %q", n.Name, n.Type)
}

func (n Notifier) postJSON(ctx context.Context, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(req, n.Name)
}

// postNtfy sends ntfy's plain-text form, using its headers so the message
// arrives on a phone with a usable title and priority.
func (n Notifier) postNtfy(ctx context.Context, msg Notification) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(msg.Text()))
	if err != nil {
		return err
	}
	req.Header.Set("Title", msg.Title())
	if msg.Severity == SevResolved {
		req.Header.Set("Priority", "low")
		req.Header.Set("Tags", "white_check_mark")
	} else {
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "rotating_light")
	}
	return do(req, n.Name)
}

// runCommand is the escape hatch: email, PagerDuty, anything with a CLI.
// The notification arrives as JSON on stdin.
func (n Notifier) runCommand(ctx context.Context, msg Notification) error {
	if len(n.Command) == 0 {
		return fmt.Errorf("notifier %q has no command", n.Name)
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, n.Command[0], n.Command[1:]...)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(cmd.Environ(),
		"PILOT_ALERT_TITLE="+msg.Title(),
		"PILOT_ALERT_SEVERITY="+string(msg.Severity),
		"PILOT_ALERT_SERVICE="+msg.Service,
		"PILOT_ALERT_HOST="+msg.Host,
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("notifier %q failed: %w: %s", n.Name, err, firstLine(string(out)))
	}
	return nil
}

func do(req *http.Request, name string) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("notifier %q: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("notifier %q returned %s", name, resp.Status)
	}
	return nil
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return head
}
