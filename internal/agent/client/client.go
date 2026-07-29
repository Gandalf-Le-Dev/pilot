// Package client talks to a pilotd agent.
//
// It is used both by `pilotd ctl` on the host and, in phase 4, by the dashboard
// — the same handlers, reached over a different socket.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// Client is a connection to one agent.
type Client struct {
	http   *http.Client
	base   string
	socket string
}

// NewUnix returns a client for an agent listening on a Unix socket.
func NewUnix(socketPath string) *Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Client{
		socket: socketPath,
		base:   "http://pilot",
		http: &http.Client{
			// No overall timeout: job long-polls legitimately block for
			// thirty seconds at a time. Per-request contexts bound them.
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// SkewError reports a CLI and agent that do not speak the same protocol.
//
// It is returned before any work is attempted, so a version mismatch can never
// leave a service half-deployed.
type SkewError struct {
	Agent, Expected int
}

func (e *SkewError) Error() string {
	return fmt.Sprintf("agent speaks protocol %d, this build speaks %d — run `pilot agent upgrade` to update it",
		e.Agent, e.Expected)
}

// NotRunningError means no agent answered on the socket.
type NotRunningError struct{ Socket string }

func (e *NotRunningError) Error() string {
	return fmt.Sprintf("no agent is listening on %s", e.Socket)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		var oe *net.OpError
		if errorsAs(err, &oe) {
			return &NotRunningError{Socket: c.socket}
		}
		return err
	}
	defer resp.Body.Close()

	// Check the handshake before the body: a protocol mismatch makes any
	// interpretation of the payload guesswork.
	if v := resp.Header.Get(proto.HeaderProtocol); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != proto.Version {
			return &SkewError{Agent: n, Expected: proto.Version}
		}
	}

	if resp.StatusCode >= 400 {
		var e proto.Error
		if err := json.NewDecoder(resp.Body).Decode(&e); err == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("agent returned %s", resp.Status)
	}

	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Info(ctx context.Context) (*proto.Info, error) {
	var out proto.Info
	if err := c.do(ctx, http.MethodGet, proto.PathInfo, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Status(ctx context.Context) (*proto.StatusResponse, error) {
	var out proto.StatusResponse
	if err := c.do(ctx, http.MethodGet, proto.PathStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Service(ctx context.Context, name string) (*proto.ServiceStatus, error) {
	var out proto.ServiceStatus
	if err := c.do(ctx, http.MethodGet, proto.PathServices+name, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Forget(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, proto.PathServices+name, nil, nil)
}

func (c *Client) Drift(ctx context.Context) (map[string]*proto.Drift, error) {
	out := map[string]*proto.Drift{}
	if err := c.do(ctx, http.MethodGet, proto.PathDrift, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) PutConfig(ctx context.Context, spec string) error {
	return c.do(ctx, http.MethodPut, proto.PathConfig, proto.ConfigRequest{Spec: spec}, nil)
}

func (c *Client) Alerts(ctx context.Context) (*proto.AlertsResponse, error) {
	var out proto.AlertsResponse
	if err := c.do(ctx, http.MethodGet, proto.PathAlerts, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Deploy(ctx context.Context, req proto.DeployRequest) (*proto.Job, error) {
	var out proto.Job
	if err := c.do(ctx, http.MethodPost, proto.PathDeploy, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Rollback(ctx context.Context, req proto.RollbackRequest) (*proto.Job, error) {
	var out proto.Job
	if err := c.do(ctx, http.MethodPost, proto.PathRollback, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Job(ctx context.Context, id string) (*proto.Job, error) {
	var out proto.Job
	if err := c.do(ctx, http.MethodGet, proto.PathJobs+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitJob blocks until the job has more than `after` events or has finished.
func (c *Client) WaitJob(ctx context.Context, id string, after int) (*proto.Job, error) {
	var out proto.Job
	path := fmt.Sprintf("%s%s?wait=1&after=%d", proto.PathJobs, id, after)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FollowJob streams a job's events to onEvent until it finishes.
//
// Returning early — because the caller's context was cancelled — does not
// affect the job. The agent owns it.
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

// errorsAs is a tiny shim so this file does not import errors purely for one
// type assertion.
func errorsAs(err error, target any) bool {
	switch t := target.(type) {
	case **net.OpError:
		for err != nil {
			if oe, ok := err.(*net.OpError); ok {
				*t = oe
				return true
			}
			u, ok := err.(interface{ Unwrap() error })
			if !ok {
				return false
			}
			err = u.Unwrap()
		}
	}
	return false
}
