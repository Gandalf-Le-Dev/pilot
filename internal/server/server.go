// Package server is `pilotd server`: the status page, and the one listener
// Pilot adds.
//
// Agents still listen on nothing. Each one dials out to this server's ingest
// address with a snapshot of its services, and the server renders what it
// was told for the public. State lives in memory only, because every report
// carries the agent's whole day: a restarted server is complete again one
// report interval later.
//
// The server also notices what no agent can: a host that has gone quiet. An
// agent whose tailnet key expired cannot report that its tailnet key expired.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

// SilenceInterval is how often hosts are checked for silence.
const SilenceInterval = 10 * time.Second

// Server holds the latest report from each host.
type Server struct {
	cfg     Config
	started time.Time
	now     func() time.Time

	hosts  []string // sorted, so token comparison visits every host in a fixed order
	hashes map[string][]byte

	mu      sync.Mutex
	reports map[string]*hostReport

	silence     alert.Rule
	silenceFrom map[string]*alert.Engine
}

type hostReport struct {
	at       time.Time
	services map[string]statuspage.ServiceReport
}

// New returns a server. A nil sender delivers through the configured
// notifiers, and a nil now reads the wall clock.
func New(cfg Config, sender alert.Sender, now func() time.Time) *Server {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if sender == nil {
		sender = registry(cfg.Notifiers)
	}

	s := &Server{
		cfg:         cfg,
		now:         now,
		started:     now(),
		hashes:      map[string][]byte{},
		reports:     map[string]*hostReport{},
		silenceFrom: map[string]*alert.Engine{},
	}

	cond, err := alert.Parse(string(alert.HostSilent))
	if err != nil {
		panic(err) // a metric this package names itself
	}
	notify := make([]string, 0, len(cfg.Notifiers))
	for name := range cfg.Notifiers {
		notify = append(notify, name)
	}
	sort.Strings(notify)
	s.silence = alert.Rule{Cond: cond, For: cfg.SilentAfter.Duration(), Notify: notify}

	for name, h := range cfg.Hosts {
		s.hosts = append(s.hosts, name)
		if sum, err := hex.DecodeString(h.TokenSHA256); err == nil {
			s.hashes[name] = sum
		}

		// One engine per host, so each message is titled by the host that
		// went quiet ("ALERT: vps") rather than by the server noticing it.
		e := alert.NewEngine(name, sender)
		e.Now = now
		e.OnError = func(rule, notifier string, err error) {
			logf("alert %q: notifier %q failed: %v", rule, notifier, err)
		}
		s.silenceFrom[name] = e
	}
	sort.Strings(s.hosts)
	return s
}

func registry(ns map[string]config.Notifier) *alert.Registry {
	var out []alert.Notifier
	for name, n := range ns {
		out = append(out, alert.Notifier{
			Name:    name,
			Type:    alert.NotifierType(n.Type),
			URL:     n.Endpoint(),
			Command: n.Command,
		})
	}
	return alert.NewRegistry(out)
}

// hostFor returns the host whose token this is.
//
// Every host's hash is compared, matched or not, each in constant time, so
// neither the comparison nor the loop says how close a guess came or which
// host it nearly was.
func (s *Server) hostFor(token string) (string, bool) {
	sum := sha256.Sum256([]byte(token))
	found := ""
	for _, name := range s.hosts {
		if subtle.ConstantTimeCompare(sum[:], s.hashes[name]) == 1 {
			found = name
		}
	}
	return found, found != ""
}

// record replaces a host's report, keeping only the services it is
// configured to publish.
func (s *Server) record(host string, rep statuspage.Report) {
	listed := s.cfg.Hosts[host].Services
	kept := map[string]statuspage.ServiceReport{}
	for _, sr := range rep.Services {
		if _, ok := listed[sr.Name]; ok {
			kept[sr.Name] = sr
		}
	}

	s.mu.Lock()
	s.reports[host] = &hostReport{at: s.now(), services: kept}
	s.mu.Unlock()
}

// lastHeard is when a host last reported, or when the server started if it
// has not reported since. Counting from the start is the restart wait: a
// fresh server owes every host one silent_after before calling it silent.
func (s *Server) lastHeard(host string) (at time.Time, reported bool) {
	if r := s.reports[host]; r != nil {
		return r.at, true
	}
	return s.started, false
}

// silentAt reports whether a host has been quiet for silent_after. Callers
// hold s.mu.
//
// This is the moment the alert fires too: its rule waits `for:` silent_after,
// counted from the same last report. So the page never says "not reporting"
// about a host nobody was told about, or the reverse.
func (s *Server) silentAt(host string, now time.Time) bool {
	at, _ := s.lastHeard(host)
	return now.Sub(at) >= s.cfg.SilentAfter.Duration()
}

// Page assembles the public page from the latest reports.
func (s *Server) Page() statuspage.Page {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	var rows []statuspage.Service
	for _, host := range s.hosts {
		at, reported := s.lastHeard(host)
		silent := s.silentAt(host, now)

		for name, label := range s.cfg.Hosts[host].Services {
			row := statuspage.Service{Label: label}
			sr, ok := statuspage.ServiceReport{}, false
			if r := s.reports[host]; r != nil {
				sr, ok = r.services[name]
			}
			switch {
			case silent:
				row.State = statuspage.NotReporting
				row.NotReportingSince = at
			case !reported || !ok:
				row.State = statuspage.AwaitingReport
			default:
				row.State = sr.State
			}
			// History outlives silence: the day before the host went quiet
			// is still the day before.
			row.History = sr.History
			rows = append(rows, row)
		}
	}
	return statuspage.Assemble(s.cfg.Title, now, rows)
}

// CheckSilence evaluates the silence rule for every host once.
//
// A host is silent the moment a report is overdue, dated from its last
// report; the rule's `for:` then holds the alert back until silent_after has
// passed since that report. A report that is merely late resets the rule
// without a word.
func (s *Server) CheckSilence(ctx context.Context) {
	now := s.now()
	readings := map[string]alert.Reading{}

	s.mu.Lock()
	for _, host := range s.hosts {
		at, reported := s.lastHeard(host)
		r := alert.Reading{HostSilent: now.Sub(at) > statuspage.ReportInterval, Since: at}
		if !reported {
			r.Detail = "no report since the status server started"
		}
		readings[host] = r
	}
	s.mu.Unlock()

	for _, host := range s.hosts {
		s.silenceFrom[host].Evaluate(ctx, []alert.Rule{s.silence}, map[string]alert.Reading{"": readings[host]})
	}
}

// WatchSilence checks for silent hosts until ctx is cancelled.
func (s *Server) WatchSilence(ctx context.Context) {
	tick := time.NewTicker(SilenceInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.CheckSilence(ctx)
	}
}

// Flush waits for queued alerts to be delivered.
func (s *Server) Flush() {
	for _, e := range s.silenceFrom {
		e.Flush()
	}
}

// logf writes a log line in the same form as the agent's, to the journal by
// way of stderr.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pilotd server: "+format+"\n", args...)
}
