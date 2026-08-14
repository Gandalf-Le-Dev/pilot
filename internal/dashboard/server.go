// Package dashboard serves the read-only fleet view.
//
// It runs on the operator's machine and dies with the terminal. Hosts are
// reached the way the CLI always reaches them — `pilotd ctl` over the SSH
// channels Pilot already holds — so nothing new listens anywhere, and the
// browser only ever talks to 127.0.0.1.
package dashboard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/dashboard/views"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// PollInterval is how often each host is asked for news. One `pilotd ctl`
// exec per host per tick — the same order of cost as `pilot top`.
const PollInterval = 5 * time.Second

// maxSeries mirrors the agent-side ring, so a long-running dashboard cannot
// hold more history than the agent would.
const maxSeries = 720

// HostClient is the one call the dashboard needs from a host.
type HostClient interface {
	Dashboard(ctx context.Context, since time.Time) (*proto.DashboardResponse, error)
}

// Source names the fleet and how to reach it. Connect returns an error
// message rather than failing the dashboard: an unreachable host is a fact
// to display, not a reason to show nothing.
type Source struct {
	Hosts   []string
	Connect func(ctx context.Context, host string) (HostClient, string)

	// Manage maps service name to its manage mode, from the fleet config —
	// the agent's status carries it too, but config is authoritative.
	Manage map[string]string
}

// Server polls the fleet and renders snapshots.
type Server struct {
	src Source

	mu    sync.RWMutex
	hosts map[string]*hostState
}

// hostState is everything one host has told us so far.
type hostState struct {
	err      string
	capacity proto.Capacity
	status   proto.StatusResponse
	alerts   []proto.AlertEvent
	deploys  map[string][]views.Deploy

	serviceSeries map[string][]proto.MetricSample
	hostSeries    []proto.MetricSample
	lastSample    time.Time
}

// New returns a server for the fleet.
func New(src Source) *Server {
	return &Server{src: src, hosts: map[string]*hostState{}}
}

// Run polls every host until ctx ends. The first round is synchronous, so
// the page that opens right after has something to say.
func (s *Server) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, host := range s.src.Hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.pollHost(ctx, host)
		}()
	}
	wg.Wait()

	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			for _, host := range s.src.Hosts {
				go s.pollHost(ctx, host)
			}
		}
	}
}

// pollHost fetches one host's news and folds it into the snapshot.
func (s *Server) pollHost(ctx context.Context, host string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	client, errMsg := s.src.Connect(ctx, host)
	if client == nil {
		s.setHostError(host, errMsg)
		return
	}

	s.mu.RLock()
	var since time.Time
	if st := s.hosts[host]; st != nil {
		since = st.lastSample
	}
	s.mu.RUnlock()

	resp, err := client.Dashboard(ctx, since)
	if err != nil {
		s.setHostError(host, err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.hosts[host]
	if st == nil {
		st = &hostState{serviceSeries: map[string][]proto.MetricSample{}}
		s.hosts[host] = st
	}
	st.err = ""
	st.capacity = resp.Capacity
	st.status = resp.Status
	st.alerts = resp.AlertEvents

	st.deploys = map[string][]views.Deploy{}
	for svc, records := range resp.Deploys {
		for _, r := range records {
			st.deploys[svc] = append(st.deploys[svc], views.Deploy{
				Host: host, Service: svc, Release: r.Release, By: r.By,
				Outcome: string(r.Outcome), FinishedAt: r.FinishedAt,
			})
		}
	}

	for svc, samples := range resp.ServiceSamples {
		st.serviceSeries[svc] = appendSeries(st.serviceSeries[svc], samples)
		st.lastSample = laterOf(st.lastSample, newest(samples))
	}
	st.hostSeries = appendSeries(st.hostSeries, resp.HostSamples)
	st.lastSample = laterOf(st.lastSample, newest(resp.HostSamples))
}

func (s *Server) setHostError(host, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.hosts[host]
	if st == nil {
		st = &hostState{serviceSeries: map[string][]proto.MetricSample{}}
		s.hosts[host] = st
	}
	st.err = msg
}

func appendSeries(ring, add []proto.MetricSample) []proto.MetricSample {
	ring = append(ring, add...)
	if len(ring) > maxSeries {
		ring = ring[len(ring)-maxSeries:]
	}
	return ring
}

func newest(samples []proto.MetricSample) time.Time {
	if len(samples) == 0 {
		return time.Time{}
	}
	return samples[len(samples)-1].At
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Snapshot renders the current view models, in fleet host order.
func (s *Server) Snapshot() views.Fleet {
	s.mu.RLock()
	defer s.mu.RUnlock()

	f := views.Fleet{GeneratedAt: time.Now()}
	for _, host := range s.src.Hosts {
		st := s.hosts[host]
		if st == nil {
			f.Hosts = append(f.Hosts, views.Host{Name: host, Err: "connecting…"})
			continue
		}
		if st.err != "" {
			f.Hosts = append(f.Hosts, views.Host{Name: host, Err: st.err})
			continue
		}

		h := views.Host{
			Name:       host,
			Capacity:   st.capacity,
			HostSeries: st.hostSeries,
			Alerts:     st.alerts,
		}
		if st.status.Disk != nil {
			h.DiskUsed = st.status.Disk.UsedPercent
		}
		for _, svc := range st.status.Services {
			manage := svc.Manage
			if m, ok := s.src.Manage[svc.Name]; ok {
				manage = m
			}
			state := string(svc.Obs.State)
			detail := svc.Obs.Detail
			if svc.Error != "" {
				state, detail = "unknown", svc.Error
			}
			h.Services = append(h.Services, views.Service{
				Name:    svc.Name,
				Runtime: svc.Runtime,
				Manage:  manage,
				State:   state,
				Release: svc.Obs.Release,
				Detail:  detail,
				Drift:   svc.Drift.Any(),
				Series:  st.serviceSeries[svc.Name],
			})
		}
		for _, records := range st.deploys {
			h.Deploys = append(h.Deploys, records...)
		}
		sort.Slice(h.Deploys, func(i, j int) bool { return h.Deploys[i].FinishedAt.After(h.Deploys[j].FinishedAt) })

		f.Hosts = append(f.Hosts, h)
	}
	return f
}

// Handler is the dashboard's HTTP surface: the page, the swap target, and
// the embedded assets. Read-only by construction — there is no mutating
// route to protect.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = views.Page(s.Snapshot()).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /fleet", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = views.Panels(s.Snapshot()).Render(r.Context(), w)
	})
	mux.HandleFunc("GET /assets/output.css", assetHandler("text/css", outputCSS))
	mux.HandleFunc("GET /assets/htmx.min.js", assetHandler("text/javascript", htmxJS))
	mux.HandleFunc("GET /assets/chart.js", assetHandler("text/javascript", chartJS))
	return mux
}

func assetHandler(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(body)
	}
}

// Serve binds the loopback listener and blocks until ctx ends.
//
// Loopback only, no auth: the security model is the same as the agent's
// socket — reaching it requires already being the operator on this machine.
// A --listen for tailnet viewing would reopen that question, so it does not
// exist yet.
func (s *Server) Serve(ctx context.Context, port int) (string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", fmt.Errorf("port %d is taken (another dashboard?): %w", port, err)
	}

	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	go func() { _ = srv.Serve(ln) }()
	return fmt.Sprintf("http://%s", ln.Addr().String()), nil
}
