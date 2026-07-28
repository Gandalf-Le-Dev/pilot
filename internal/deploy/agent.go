package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gandalfledev/pilot/internal/agent/remote"
	"github.com/gandalfledev/pilot/internal/config"
	"github.com/gandalfledev/pilot/internal/runtime"
	"github.com/gandalfledev/pilot/internal/transport/proto"
)

// activateViaAgent hands the commit point to the host's daemon.
//
// This is the difference between the safety being implemented and it being on.
// Staging stays here because the built bytes are here; everything after it —
// the symlink swap, the health check, the rollback when that fails — runs in a
// process that lives on the machine. Closing a laptop now interrupts a
// spectator, not a deploy.
func (e *Executor) activateViaAgent(ctx context.Context, rc *remote.Client, p *Plan, hp HostPlan, started time.Time, out Outcome) Outcome {
	req := proto.DeployRequest{
		Service:      p.Service.Name,
		Release:      p.Release,
		Spec:         e.Spec,
		Route:        p.Snippet,
		RouteAction:  toProtoRoute(hp.Route),
		Verify:       !e.SkipVerify,
		KeepReleases: p.Service.KeepReleases,
		By:           e.By,
	}

	job, err := rc.Deploy(ctx, req)
	if err != nil {
		out.Err = fmt.Errorf("handing the deploy to the agent on %s: %w", hp.Host, err)
		return out
	}
	e.Log("  %s: job %s running on the host", hp.Host, job.ID)

	final, err := rc.FollowJob(ctx, job.ID, func(ev proto.JobEvent) {
		e.Log("  %s: %s", hp.Host, ev.Message)
	})
	if err != nil {
		// Losing the stream is not losing the deploy, and the message has to
		// make that unmistakable — otherwise the natural reaction is to panic
		// and start deploying again on top of a job that is still running.
		out.Err = fmt.Errorf("lost contact with %s while watching job %s\n"+
			"the deploy is still running on the host and will finish or roll back on its own\n"+
			"check it with: pilot status %s", hp.Host, job.ID, p.Service.Name)
		return out
	}

	out.RolledBack = final.RolledBack
	if final.State != proto.JobSucceeded {
		out.Err = errors.New(final.Error)
		if out.Err.Error() == "" {
			out.Err = fmt.Errorf("job %s failed", final.ID)
		}
		return out
	}

	out.Succeeded = true
	return out
}

func toProtoRoute(a RouteAction) proto.RouteAction {
	switch a {
	case RouteInstall:
		return proto.RouteInstall
	case RouteUpdate:
		return proto.RouteUpdate
	case RouteNone:
		return proto.RouteNone
	}
	return ""
}

// RollbackViaAgent asks the host's daemon to return a service to an earlier
// release.
func RollbackViaAgent(ctx context.Context, rc *remote.Client, service, to, by string, log Logger) error {
	job, err := rc.Rollback(ctx, proto.RollbackRequest{Service: service, To: to, By: by})
	if err != nil {
		return err
	}

	final, err := rc.FollowJob(ctx, job.ID, func(ev proto.JobEvent) {
		log("    %s", ev.Message)
	})
	if err != nil {
		return fmt.Errorf("lost contact while watching job %s; the rollback continues on the host: %w", job.ID, err)
	}
	if final.State != proto.JobSucceeded {
		if final.Error != "" {
			return errors.New(final.Error)
		}
		return fmt.Errorf("job %s failed", final.ID)
	}
	return nil
}

// SpecFor renders a service definition for the agent to cache.
//
// It goes through the same marshal/parse pair the agent uses, so a definition
// that the CLI can send is by construction one the agent can read back.
func SpecFor(s *config.Service) (string, error) {
	body, err := config.MarshalService(s)
	if err != nil {
		return "", err
	}
	// Parse it back immediately: shipping a spec the agent will reject would
	// leave the host unable to monitor the very service it just deployed.
	if _, err := config.ParseService(body, s.Name+".yaml"); err != nil {
		return "", fmt.Errorf("service definition for %q does not round-trip: %w", s.Name, err)
	}
	return string(body), nil
}

// compile-time reminder that the agent path and the direct path operate on the
// same targets.
var _ = (*runtime.Target)(nil)
