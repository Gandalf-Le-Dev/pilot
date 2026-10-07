package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

// Pushing the public snapshot to the status server.
//
// The agent dials out and nothing dials in: the server's address arrives in
// the pushed config, the agent posts to it, and the agent's own surface stays
// a Unix socket. There is no queue and no retry beyond the next tick. Every
// report carries the whole day, so a report that failed is replaced, not
// lost, thirty seconds later.

// pushTimeout bounds one report, well inside the interval, so a status server
// that accepts and never answers cannot stack reports up behind it.
var pushTimeout = 10 * time.Second

var pushClient = &http.Client{
	// A redirect from the ingest address would carry the bearer token
	// somewhere the operator never configured.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// pushReport posts one snapshot of the listed services.
//
// The body is built from statuspage types alone, and those have no field for
// an error, a detail line, a release or a URL. What a runtime says about a
// service stays on this host.
func (a *Agent) pushReport(ctx context.Context, t *config.ReportTarget) error {
	rep := statuspage.Report{Host: a.Host, Services: a.HealthReport(t.Services)}
	if rep.Services == nil {
		rep.Services = []statuspage.ServiceReport{}
	}
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	if len(body) > statuspage.MaxReportBytes {
		return fmt.Errorf("report is %d bytes, over the server's %d", len(body), statuspage.MaxReportBytes)
	}

	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.Token)

	resp, err := pushClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("status server answered %s", resp.Status)
	}
	return nil
}

// pushState logs the first failure and the recovery, not every attempt: a
// status server down for a night is two journal lines, not two thousand.
type pushState struct{ failing bool }

func (p *pushState) record(err error, url string) {
	switch {
	case err != nil && !p.failing:
		logf("reporting to %s failed, retrying every %s: %v", url, HealthInterval, err)
		p.failing = true
	case err == nil && p.failing:
		logf("reporting to %s again", url)
		p.failing = false
	}
}
