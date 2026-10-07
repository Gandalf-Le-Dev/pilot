package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/alert"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

// recorder captures notifications instead of sending them.
type recorder struct {
	mu   sync.Mutex
	sent []alert.Notification
}

func (r *recorder) Send(_ context.Context, _ string, msg alert.Notification) error {
	r.mu.Lock()
	r.sent = append(r.sent, msg)
	r.mu.Unlock()
	return nil
}

func (r *recorder) all() []alert.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]alert.Notification(nil), r.sent...)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

const secret = "fleet-secret"

// Host and service names a leak would show. None may reach the public.
const (
	quietHost  = "vps-tailnet-7f3a"
	otherHost  = "ks-private-name"
	notesName  = "docmost"
	notesLabel = "Notes"
)

func testServer(t *testing.T) (*Server, *recorder, *clock) {
	t.Helper()
	cfg := Config{
		Title:       "mroc.me",
		SilentAfter: config.Duration(90 * time.Second),
		Notifiers:   map[string]config.Notifier{"discord": {Type: "discord", URL: "https://discord.example/hook"}},
		Hosts: map[string]HostConfig{
			quietHost: {
				TokenSHA256: statuspage.TokenHash(statuspage.Token(secret, quietHost)),
				Services:    map[string]string{notesName: notesLabel},
			},
			otherHost: {
				TokenSHA256: statuspage.TokenHash(statuspage.Token(secret, otherHost)),
				Services:    map[string]string{"site": "Website"},
			},
		},
	}
	rec := &recorder{}
	clk := &clock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	return New(cfg, rec, clk.now), rec, clk
}

func report(host string, services ...statuspage.ServiceReport) statuspage.Report {
	return statuspage.Report{Host: host, Services: services}
}

func healthy(name string, at time.Time) statuspage.ServiceReport {
	return statuspage.ServiceReport{Name: name, State: statuspage.Up, History: statuspage.History{
		End: statuspage.BucketStart(at), States: []statuspage.State{statuspage.Up, statuspage.Up},
	}}
}

func post(t *testing.T, s *Server, token string, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, statuspage.ReportPath, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.IngestHandler().ServeHTTP(w, req)
	return w.Result()
}

func postReport(t *testing.T, s *Server, host string, rep statuspage.Report) *http.Response {
	t.Helper()
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	return post(t, s, statuspage.Token(secret, host), string(b))
}

func page(t *testing.T, s *Server) statuspage.Page {
	t.Helper()
	w := httptest.NewRecorder()
	s.PublicHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /status.json = %d", w.Code)
	}
	var p statuspage.Page
	if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func row(t *testing.T, p statuspage.Page, label string) statuspage.Service {
	t.Helper()
	for _, s := range p.Services {
		if s.Label == label {
			return s
		}
	}
	t.Fatalf("no %q row on the page: %+v", label, p.Services)
	return statuspage.Service{}
}

func TestIngestRejectsBadTokens(t *testing.T) {
	s, _, clk := testServer(t)
	body, _ := json.Marshal(report(quietHost, healthy(notesName, clk.now())))

	for name, token := range map[string]string{
		"none":                "",
		"wrong secret":        statuspage.Token("guessed", quietHost),
		"the hash, replayed":  statuspage.TokenHash(statuspage.Token(secret, quietHost)),
		"a host not in fleet": statuspage.Token(secret, "intruder"),
	} {
		t.Run(name, func(t *testing.T) {
			resp := post(t, s, token, string(body))
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, statuspage.ReportPath, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Basic "+statuspage.Token(secret, quietHost))
	w := httptest.NewRecorder()
	s.IngestHandler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a token outside a Bearer header = %d, want 401", w.Code)
	}
}

func TestIngestRejectsOversizedReports(t *testing.T) {
	s, _, _ := testServer(t)
	huge := `{"host":"` + quietHost + `","services":[],"pad":"` + strings.Repeat("x", MaxReportBytes) + `"}`
	resp := post(t, s, statuspage.Token(secret, quietHost), huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

// The limit is inclusive: a report of exactly MaxReportBytes is accepted, so
// an agent sizing its report against the constant is never refused for it.
func TestIngestAcceptsAReportOfExactlyTheLimit(t *testing.T) {
	s, _, clk := testServer(t)
	body, _ := json.Marshal(report(quietHost, healthy(notesName, clk.now())))
	padded := string(body) + strings.Repeat(" ", MaxReportBytes-len(body))
	if len(padded) != MaxReportBytes {
		t.Fatalf("fixture is %d bytes", len(padded))
	}
	if resp := post(t, s, statuspage.Token(secret, quietHost), padded); resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 at exactly the limit", resp.StatusCode)
	}
	if resp := post(t, s, statuspage.Token(secret, quietHost), padded+" "); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 one byte over", resp.StatusCode)
	}
}

// One bucket of skew is tolerated; more is a clock that is wrong.
func TestIngestAcceptsOneBucketOfSkew(t *testing.T) {
	s, _, clk := testServer(t)
	ahead := healthy(notesName, clk.now().Add(statuspage.BucketWidth))
	if resp := postReport(t, s, quietHost, report(quietHost, ahead)); resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for a history one bucket ahead", resp.StatusCode)
	}
}

func TestIngestRejectsMalformedReports(t *testing.T) {
	s, _, clk := testServer(t)
	end := statuspage.BucketStart(clk.now())
	token := statuspage.Token(secret, quietHost)

	for name, body := range map[string]string{
		"not json":         `{"host":`,
		"trailing data":    `{"host":"` + quietHost + `","services":[]} {}`,
		"unknown field":    `{"host":"` + quietHost + `","services":[{"name":"docmost","state":"down","detail":"dial tcp 127.0.0.1:5432: refused","history":{"end":0,"states":[]}}]}`,
		"unknown state":    `{"host":"` + quietHost + `","services":[{"name":"docmost","state":"on fire","history":{"end":0,"states":[]}}]}`,
		"server verdict":   `{"host":"` + quietHost + `","services":[{"name":"docmost","state":"not_reporting","history":{"end":0,"states":[]}}]}`,
		"bad bucket state": `{"host":"` + quietHost + `","services":[{"name":"docmost","state":"up","history":{"end":` + itoa(end) + `,"states":["up","meh"]}}]}`,
		"future end":       `{"host":"` + quietHost + `","services":[{"name":"docmost","state":"up","history":{"end":` + itoa(end+2*300) + `,"states":["up"]}}]}`,
		"unaligned end":    `{"host":"` + quietHost + `","services":[{"name":"docmost","state":"up","history":{"end":` + itoa(end+7) + `,"states":["up"]}}]}`,
		"too much history": `{"host":"` + quietHost + `","services":[{"name":"docmost","state":"up","history":{"end":` + itoa(end) + `,"states":[` + strings.TrimSuffix(strings.Repeat(`"up",`, statuspage.HistoryBuckets+1), ",") + `]}}]}`,
		"unnamed service":  `{"host":"` + quietHost + `","services":[{"name":"","state":"up","history":{"end":0,"states":[]}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := post(t, s, token, body)
			if resp.StatusCode != http.StatusBadRequest {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d (%s), want 400", resp.StatusCode, b)
			}
		})
	}
}

// A valid token speaking for another host is refused, so one compromised
// host cannot keep a silent neighbour looking alive.
func TestIngestRejectsAnotherHostsReport(t *testing.T) {
	s, _, clk := testServer(t)
	b, _ := json.Marshal(report(otherHost, healthy("site", clk.now())))
	resp := post(t, s, statuspage.Token(secret, quietHost), string(b))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestReportReachesThePage(t *testing.T) {
	s, _, clk := testServer(t)

	if got := row(t, page(t, s), notesLabel).State; got != statuspage.AwaitingReport {
		t.Errorf("before any report: %s, want awaiting_report", got)
	}

	down := healthy(notesName, clk.now())
	down.State = statuspage.Down
	if resp := postReport(t, s, quietHost, report(quietHost, down)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	p := page(t, s)
	notes := row(t, p, notesLabel)
	if notes.State != statuspage.Down || len(notes.History.States) != 2 {
		t.Errorf("notes = %+v, want down with its history", notes)
	}
	if p.State != statuspage.Down {
		t.Errorf("page state = %s, want the worst row", p.State)
	}
	if got := row(t, p, "Website").State; got != statuspage.AwaitingReport {
		t.Errorf("a host that has not reported: %s, want awaiting_report", got)
	}
}

// The silence alert fires once the host has been quiet for silent_after,
// says which host, and resolves when it reports again.
func TestSilenceAlertFiresAfterTimeout(t *testing.T) {
	s, rec, clk := testServer(t)
	ctx := context.Background()
	postReport(t, s, quietHost, report(quietHost, healthy(notesName, clk.now())))
	postReport(t, s, otherHost, report(otherHost, healthy("site", clk.now())))
	reported := clk.now()

	clk.add(45 * time.Second)
	s.CheckSilence(ctx) // overdue, not yet silent
	clk.add(15 * time.Second)
	postReport(t, s, otherHost, report(otherHost, healthy("site", clk.now())))
	clk.add(29 * time.Second)
	s.CheckSilence(ctx)
	s.Flush()
	if n := len(rec.all()); n != 0 {
		t.Fatalf("fired at 89s of a 90s silence (%d sent)", n)
	}

	clk.add(2 * time.Second)
	s.CheckSilence(ctx)
	s.Flush()
	sent := rec.all()
	if len(sent) != 1 {
		t.Fatalf("got %d notifications, want one for the quiet host: %+v", len(sent), sent)
	}
	if sent[0].Severity != alert.SevFiring || sent[0].Title() != "ALERT: "+quietHost {
		t.Errorf("notification = %q / %q", sent[0].Severity, sent[0].Title())
	}
	if !sent[0].Since.Equal(reported) {
		t.Errorf("the alert dates the silence from %s, want the last report at %s", sent[0].Since, reported)
	}

	notes := row(t, page(t, s), notesLabel)
	if notes.State != statuspage.NotReporting || !notes.NotReportingSince.Equal(reported) {
		t.Errorf("notes = %s since %s, want not_reporting since %s", notes.State, notes.NotReportingSince, reported)
	}
	if len(notes.History.States) == 0 {
		t.Error("silence erased the history the host reported before it went quiet")
	}

	// Still quiet: the cooldown holds the repeat back.
	clk.add(time.Minute)
	postReport(t, s, otherHost, report(otherHost, healthy("site", clk.now())))
	s.CheckSilence(ctx)
	s.Flush()
	if n := len(rec.all()); n != 1 {
		t.Errorf("repeated within the cooldown (%d sent)", n)
	}

	postReport(t, s, quietHost, report(quietHost, healthy(notesName, clk.now())))
	s.CheckSilence(ctx)
	s.Flush()
	sent = rec.all()
	if len(sent) != 2 || sent[1].Severity != alert.SevResolved {
		t.Errorf("want a resolution once the host reports again, got %+v", sent)
	}
}

// After a restart the server knows nothing, and must not page about every
// host before any of them has had the chance to report.
func TestRestartWaitsBeforeSilenceAlert(t *testing.T) {
	s, rec, clk := testServer(t)
	ctx := context.Background()

	clk.add(89 * time.Second)
	s.CheckSilence(ctx)
	s.Flush()
	if n := len(rec.all()); n != 0 {
		t.Fatalf("paged %d times within silent_after of starting", n)
	}
	if got := row(t, page(t, s), notesLabel).State; got != statuspage.AwaitingReport {
		t.Errorf("state = %s, want awaiting_report during the restart wait", got)
	}

	postReport(t, s, otherHost, report(otherHost, healthy("site", clk.now())))
	clk.add(2 * time.Second)
	s.CheckSilence(ctx)
	s.Flush()
	sent := rec.all()
	if len(sent) != 1 || sent[0].Host != quietHost {
		t.Fatalf("want one alert, for the host that never reported: %+v", sent)
	}
	if !strings.Contains(sent[0].Text(), "since the status server started") {
		t.Errorf("the alert should say the host has not reported since the restart:\n%s", sent[0].Text())
	}
}

func TestPublicHandlerIsReadOnly(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.PublicHandler()

	for _, path := range []string{"/", "/status.json", "/healthz"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader("{}")))
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, path, w.Code)
			}
		}
	}

	// The ingest route does not exist on the public side at all.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, statuspage.ReportPath, strings.NewReader("{}")))
	if w.Code != http.StatusNotFound {
		t.Errorf("POST %s on the public handler = %d, want 404", statuspage.ReportPath, w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("GET / = %d with CSP %q", w.Code, w.Header().Get("Content-Security-Policy"))
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "<script") {
		t.Error("the page carries a script")
	}
}

// The leak guard. Everything an agent could get wrong, or an attacker with a
// token could try, is sent; none of it may reach the public page or JSON.
func TestPublicOutputLeaksNothing(t *testing.T) {
	s, _, clk := testServer(t)

	smuggled := statuspage.ServiceReport{
		Name: "dial tcp 127.0.0.1:5432: connection refused (release 0042-9f3ac1b)", State: statuspage.Down,
	}
	postReport(t, s, quietHost, report(quietHost, healthy(notesName, clk.now()), smuggled))
	postReport(t, s, otherHost, report(otherHost, healthy("site", clk.now())))
	clk.add(5 * time.Minute) // let one host go silent, so its "since" is rendered too
	postReport(t, s, otherHost, report(otherHost, healthy("site", clk.now())))

	for _, path := range []string{"/", "/status.json"} {
		w := httptest.NewRecorder()
		s.PublicHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		body := w.Body.String()
		if !strings.Contains(body, notesLabel) {
			t.Fatalf("%s does not show the labelled service at all:\n%s", path, body)
		}
		for _, leak := range []string{
			quietHost, otherHost, notesName,
			"127.0.0.1", "localhost", "connection refused", "0042-9f3ac1b",
			statuspage.Token(secret, quietHost),
		} {
			if strings.Contains(body, leak) {
				t.Errorf("%s leaks %q:\n%s", path, leak, body)
			}
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
