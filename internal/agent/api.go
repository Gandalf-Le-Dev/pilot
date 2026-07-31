package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

// Handler returns the agent's HTTP API.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+proto.PathInfo, a.handleInfo)
	mux.HandleFunc("GET "+proto.PathStatus, a.handleStatus)
	mux.HandleFunc("GET "+proto.PathDrift, a.handleDrift)
	mux.HandleFunc("GET "+proto.PathAlerts, a.handleAlerts)
	mux.HandleFunc("PUT "+proto.PathConfig, a.handleConfig)
	mux.HandleFunc("POST "+proto.PathDeploy, a.handleDeploy)
	mux.HandleFunc("POST "+proto.PathRollback, a.handleRollback)
	mux.HandleFunc("GET "+proto.PathJobs+"{id}", a.handleJob)
	mux.HandleFunc("GET "+proto.PathServices+"{name}", a.handleService)
	mux.HandleFunc("PUT "+proto.PathServices+"{name}", a.handlePutService)
	mux.HandleFunc("DELETE "+proto.PathServices+"{name}", a.handleForget)

	// Stamp the protocol version on every response, so a skewed CLI finds out
	// on its first call rather than partway through a deploy.
	return protocolHeader(mux)
}

func protocolHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, proto.Error{Error: err.Error()})
}

func (a *Agent) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, proto.Info{
		Protocol:  proto.Version,
		Build:     a.Build,
		Host:      a.Host,
		StartedAt: a.StartedAt,
		Services:  len(a.ServiceNames()),
	})
}

func (a *Agent) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := proto.StatusResponse{Host: a.Host}

	for _, name := range a.ServiceNames() {
		resp.Services = append(resp.Services, a.statusFor(r.Context(), name))
	}
	if d, err := a.diskUsage(); err == nil {
		resp.Disk = d
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) statusFor(ctx context.Context, name string) proto.ServiceStatus {
	s, _ := a.Service(name)
	out := proto.ServiceStatus{Name: name}
	if s == nil {
		out.Error = "no cached definition"
		return out
	}
	out.Runtime = string(s.Runtime)
	out.Manage = string(s.Manage)

	rt, err := RuntimeFor(s)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	obs, err := rt.Observe(ctx, a.Target(s, ""))
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Obs = obs

	if d := a.Drift(name); d != nil && (d.config || d.route) {
		out.Drift = &proto.Drift{
			DetectedAt: d.detectedAt, Release: d.release,
			Config: d.config, Route: d.route, Detail: d.detail,
		}
	}
	return out
}

func (a *Agent) handleService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := a.Service(name); !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such service %q on this host", name))
		return
	}
	writeJSON(w, http.StatusOK, a.statusFor(r.Context(), name))
}

// handlePutService caches a definition without deploying anything.
//
// The agent uses a service's spec long after the deploy that delivered it — to
// observe, probe health, detect drift, and evaluate alerts. Those fields do not
// affect the release hash, so changing one alone leaves the deploy a no-op and,
// before this existed, the agent kept acting on the previous spec indefinitely.
//
// That is not theoretical. A service reverted from blue-green to recreate went
// on being observed as blue-green: the agent looked for a compose project that
// did not exist and reported a service that was serving traffic as stopped —
// which a `service.down` rule would have escalated at 3am.
func (a *Agent) handlePutService(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	s, err := a.PutService(string(body))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.Name != r.PathValue("name") {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("spec is for %q but was pushed as %q", s.Name, r.PathValue("name")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleForget(w http.ResponseWriter, r *http.Request) {
	if err := a.ForgetService(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleDrift(w http.ResponseWriter, r *http.Request) {
	out := map[string]*proto.Drift{}
	for _, name := range a.ServiceNames() {
		if d := a.Drift(name); d != nil && (d.config || d.route) {
			out[name] = &proto.Drift{
				DetectedAt: d.detectedAt, Release: d.release,
				Config: d.config, Route: d.route, Detail: d.detail,
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Agent) handleAlerts(w http.ResponseWriter, r *http.Request) {
	firing := a.FiringAlerts()
	if firing == nil {
		firing = []string{}
	}
	writeJSON(w, http.StatusOK, proto.AlertsResponse{Host: a.Host, Firing: firing})
}

func (a *Agent) handleConfig(w http.ResponseWriter, r *http.Request) {
	var req proto.ConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.PutFleetConfig(req.Spec); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req proto.DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	job, err := a.StartDeploy(req)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (a *Agent) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req proto.RollbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	job, err := a.StartRollback(req)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// handleJob returns a job, optionally waiting for it to change.
//
// Long-polling rather than streaming keeps the client simple and makes
// disconnection a non-event: the job neither knows nor cares that anyone was
// watching.
func (a *Agent) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	if r.URL.Query().Get("wait") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		job, ok := a.jobs.Wait(ctx, id, after)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("no such job %q", id))
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}

	job, ok := a.jobs.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such job %q", id))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// diskUsage reports headroom on the volume holding releases.
func (a *Agent) diskUsage() (*proto.DiskUsage, error) {
	res, err := a.Exec.Run(context.Background(), "df -P "+a.Layout.Root+" 2>/dev/null || df -P /")
	if err != nil || !res.OK() {
		return nil, fmt.Errorf("df failed")
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("unexpected df output")
	}
	for _, f := range strings.Fields(lines[len(lines)-1]) {
		if pct, ok := strings.CutSuffix(f, "%"); ok {
			n, err := strconv.Atoi(pct)
			if err != nil {
				continue
			}
			return &proto.DiskUsage{Path: a.Layout.Root, UsedPercent: n}, nil
		}
	}
	return nil, fmt.Errorf("no percentage in df output")
}

// Serve listens on a Unix socket until ctx is cancelled.
//
// A Unix socket, mode 0600, is the whole security model: reaching the agent
// requires already being on the box with the right uid. There is no port, no
// certificate, and nothing new exposed to the network.
func (a *Agent) Serve(ctx context.Context, socketPath string) error {
	// sun_path is 104 bytes on darwin and 108 on Linux. Exceeding it surfaces
	// as a bare "invalid argument" from bind(2), which is a miserable thing to
	// debug, so say what is actually wrong.
	if len(socketPath) >= 100 {
		return fmt.Errorf("socket path is %d bytes, too long for a unix socket (limit ~104): %s",
			len(socketPath), socketPath)
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return err
	}
	// A socket left behind by a killed daemon would otherwise make every
	// restart fail with "address already in use".
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing stale socket %s: %w", socketPath, err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	defer os.Remove(socketPath)

	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("securing %s: %w", socketPath, err)
	}

	srv := &http.Server{
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logf("listening on %s (protocol %d, build %s)", socketPath, proto.Version, a.Build)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
