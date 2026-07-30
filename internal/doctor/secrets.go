package doctor

import (
	"context"
	"fmt"

	"github.com/Gandalf-Le-Dev/pilot/internal/redact"
)

// LogSample is a bounded tail of one service's output, plus whatever secret
// values Pilot supplied to it.
type LogSample struct {
	Text string

	// Known maps names to values Pilot gave the service, so a match can be
	// reported by name. Only values already known without side effects belong
	// here: resolving a `${cmd:…}` reference during a read-only check could
	// prompt for a keychain or fail for reasons that have nothing to do with
	// the host being inspected.
	Known map[string]string
}

// LogSampler fetches recent output for a service on a host.
//
// An interface for the same reason as Agents: reading logs needs a runtime
// adapter and a target, and doctor has no business assembling either. The
// command wires one in; nil skips the check.
type LogSampler interface {
	Sample(ctx context.Context, service, host string, lines int) (*LogSample, error)
}

// sampleLines is how much output to inspect per service.
//
// Enough that a leak repeated on every request shows up immediately, small
// enough that `doctor` does not become slow or pull down a large amount of a
// user's log data to scan it.
const sampleLines = 200

// checkLogSecrets reports credentials appearing in service logs.
//
// This check treats the cause rather than a symptom, which is why it exists and
// why it came before log redaction. Redaction would clean one read path; the
// credential is still written to the host on every request and stays in
// `docker logs`, in journald, and in any backup of them. Telling someone their
// application is logging its own API key lets them fix the application.
//
// It is not fixable by Pilot, deliberately. Nothing Pilot can do to a host stops
// a service writing a secret to stdout.
func checkLogSecrets(ctx context.Context, env *Env) []Finding {
	if env.Logs == nil {
		return nil
	}

	var out []Finding
	for _, host := range env.Fleet.HostNames() {
		if env.Client(host) == nil {
			continue // unreachable hosts are already reported once
		}

		for _, s := range env.ServicesOn(host) {
			sample, err := env.Logs.Sample(ctx, s.Name, host, sampleLines)
			if err != nil || sample == nil {
				// A service with no readable logs is not a finding. It is
				// usually one that has not started, which other checks report.
				continue
			}

			matches := redact.Scan(sample.Text, sample.Known)
			if len(matches) == 0 {
				continue
			}

			out = append(out, Finding{
				Status: StatusWarn, Scope: ScopeHost, Host: host,
				Title: fmt.Sprintf("%s logs credentials (%s)", s.Name, redact.Summarise(matches)),
				Detail: "These appear in the last " + fmt.Sprint(sampleLines) + " lines of output, so they are " +
					"also in `docker logs` or journald on the host and in any backup of them. " +
					"Anything that reads the logs reads the credential — including a person you " +
					"send them to, and any AI you ask to diagnose a problem.",
				Hint: "stop the service logging it (often a request URI carrying a query parameter), " +
					"then rotate the credential — it should be assumed exposed",
			})
		}
	}
	return out
}
