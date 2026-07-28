// Package remote reaches an agent from the operator's machine.
//
// It does so by running `pilotd ctl` over the existing SSH connection rather
// than by opening a port. The agent's socket stays 0600 and local; SSH remains
// the only way in, which is the whole of Pilot's phase 1–3 security model.
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gandalfledev/pilot/internal/release"
	"github.com/gandalfledev/pilot/internal/transport"
	"github.com/gandalfledev/pilot/internal/transport/proto"
)

// Client talks to the agent on one host.
type Client struct {
	Exec   transport.Executor
	Binary string
	Socket string
	Host   string
}

// New returns a client using the default agent location.
func New(exec transport.Executor, host string, layout release.Layout) *Client {
	return &Client{
		Exec:   exec,
		Binary: layout.Agent(),
		Socket: proto.DefaultSocket,
		Host:   host,
	}
}

// NotInstalledError means the host has no agent, so the caller should fall back
// to driving the host directly over SSH.
type NotInstalledError struct{ Host string }

func (e *NotInstalledError) Error() string {
	return fmt.Sprintf("no pilot agent on %s", e.Host)
}

// NotRunningError means the binary is present but the daemon is not answering.
type NotRunningError struct {
	Host   string
	Detail string
}

func (e *NotRunningError) Error() string {
	return fmt.Sprintf("the agent on %s is installed but not responding: %s", e.Host, e.Detail)
}

// SkewError means the agent speaks a different protocol version.
type SkewError struct {
	Host            string
	Agent, Expected int
}

func (e *SkewError) Error() string {
	return fmt.Sprintf("the agent on %s speaks protocol %d, this build speaks %d — run `pilot bootstrap %s` to update it",
		e.Host, e.Agent, e.Expected, e.Host)
}

// ctl runs a ctl verb and decodes its JSON output.
func (c *Client) ctl(ctx context.Context, out any, stdin []byte, args ...string) error {
	argv := append([]string{c.Binary, "ctl"}, args...)
	argv = append(argv, "--socket", c.Socket)
	cmd := transport.Join(argv...)

	var (
		res transport.Result
		err error
	)
	if stdin != nil {
		res, err = c.Exec.RunInput(ctx, cmd, stdin)
	} else {
		res, err = c.Exec.Run(ctx, cmd)
	}
	if err != nil {
		return err
	}

	if !res.OK() {
		detail := strings.TrimSpace(res.Stderr)
		switch {
		case strings.Contains(detail, "not found"), strings.Contains(detail, "No such file"):
			return &NotInstalledError{Host: c.Host}
		case strings.Contains(detail, "no agent is listening"):
			return &NotRunningError{Host: c.Host, Detail: detail}
		}
		return fmt.Errorf("%s", transport.FirstLine(detail))
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(res.Stdout), out); err != nil {
		return fmt.Errorf("unreadable response from the agent on %s: %w", c.Host, err)
	}
	return nil
}

// Info returns the agent's identity, and is also the version handshake.
//
// Every other call goes through Check first, so a protocol mismatch is caught
// before any work starts rather than partway through a deploy.
func (c *Client) Info(ctx context.Context) (*proto.Info, error) {
	var out proto.Info
	if err := c.ctl(ctx, &out, nil, "info"); err != nil {
		return nil, err
	}
	return &out, nil
}

// Check verifies that an agent is present and speaks our protocol.
func (c *Client) Check(ctx context.Context) (*proto.Info, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	if info.Protocol != proto.Version {
		return nil, &SkewError{Host: c.Host, Agent: info.Protocol, Expected: proto.Version}
	}
	return info, nil
}

// Available reports whether a usable agent is present, for callers that fall
// back to driving the host directly.
func (c *Client) Available(ctx context.Context) bool {
	_, err := c.Check(ctx)
	return err == nil
}

func (c *Client) Status(ctx context.Context) (*proto.StatusResponse, error) {
	var out proto.StatusResponse
	if err := c.ctl(ctx, &out, nil, "status"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Drift(ctx context.Context) (map[string]*proto.Drift, error) {
	out := map[string]*proto.Drift{}
	if err := c.ctl(ctx, &out, nil, "drift"); err != nil {
		return nil, err
	}
	return out, nil
}

// PutConfig pushes the host-wide notifiers and alert rules.
func (c *Client) PutConfig(ctx context.Context, spec string) error {
	body, err := json.Marshal(proto.ConfigRequest{Spec: spec})
	if err != nil {
		return err
	}
	return c.ctl(ctx, nil, body, "config")
}

// Alerts reports which rules are currently firing on the host.
func (c *Client) Alerts(ctx context.Context) (*proto.AlertsResponse, error) {
	var out proto.AlertsResponse
	if err := c.ctl(ctx, &out, nil, "alerts"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Deploy(ctx context.Context, req proto.DeployRequest) (*proto.Job, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var out proto.Job
	if err := c.ctl(ctx, &out, body, "deploy"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Rollback(ctx context.Context, req proto.RollbackRequest) (*proto.Job, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var out proto.Job
	if err := c.ctl(ctx, &out, body, "rollback"); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitJob blocks until the job has more than `after` events or finishes.
func (c *Client) WaitJob(ctx context.Context, id string, after int) (*proto.Job, error) {
	var out proto.Job
	err := c.ctl(ctx, &out, nil, "job", id, "--wait", "--after", fmt.Sprint(after))
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FollowJob streams a job's events until it finishes.
//
// Giving up here — a cancelled context, a dropped connection — does not touch
// the job. The agent owns it, and it will finish or roll back either way.
func (c *Client) FollowJob(ctx context.Context, id string, onEvent func(proto.JobEvent)) (*proto.Job, error) {
	seen := 0
	for {
		job, err := c.WaitJob(ctx, id, seen)
		if err != nil {
			return nil, err
		}
		for _, e := range job.Events[min(seen, len(job.Events)):] {
			onEvent(e)
		}
		seen = len(job.Events)

		if job.State.Done() {
			return job, nil
		}
		if err := ctx.Err(); err != nil {
			return job, err
		}
	}
}
