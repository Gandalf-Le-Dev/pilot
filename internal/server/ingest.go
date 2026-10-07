package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/statuspage"
)

// IngestHandler accepts reports from agents. It is the only handler bound to
// a non-loopback address.
func (s *Server) IngestHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+statuspage.ReportPath, s.handleReport)
	return mux
}

// handleReport authenticates before it reads the body, so a caller without a
// token costs a header comparison and nothing more. Error bodies say which
// rule was broken and never echo what was sent.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	host, known := s.hostFor(token)
	if !ok || token == "" || !known {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pilot"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, statuspage.MaxReportBytes+1))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if len(body) > statuspage.MaxReportBytes {
		http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
		return
	}

	var rep statuspage.Report
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		http.Error(w, "malformed report", http.StatusBadRequest)
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		http.Error(w, "malformed report", http.StatusBadRequest)
		return
	}

	// A valid token speaking for another host is a stolen or misdeployed
	// token either way, and accepting it would let one host vouch for
	// another's silence.
	if rep.Host != host {
		http.Error(w, "report is not for this token's host", http.StatusForbidden)
		return
	}
	if err := validReport(rep, s.now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.record(host, rep)
	w.WriteHeader(http.StatusNoContent)
}

// validReport checks every enum and bound. The messages name the rule, not
// the offending value.
//
// A history may end one bucket ahead of the server's clock, to allow for
// skew, and no further: a bucket from tomorrow would sit on the page as
// today's newest and push the real day out of the merge window.
func validReport(rep statuspage.Report, now time.Time) error {
	width := int64(statuspage.BucketWidth / time.Second)
	latest := statuspage.BucketStart(now) + width
	seen := map[string]bool{}
	for _, sr := range rep.Services {
		switch {
		case sr.Name == "":
			return fmt.Errorf("a service has no name")
		case seen[sr.Name]:
			return fmt.Errorf("a service is reported twice")
		case !sr.State.Valid():
			return fmt.Errorf("a service has an unknown state")
		case len(sr.History.States) > statuspage.HistoryBuckets:
			return fmt.Errorf("a history is longer than %d buckets", statuspage.HistoryBuckets)
		case sr.History.End < 0 || sr.History.End%width != 0:
			return fmt.Errorf("a history does not end on a bucket boundary")
		case sr.History.End > latest:
			return fmt.Errorf("a history ends in the future")
		}
		seen[sr.Name] = true
		for _, st := range sr.History.States {
			if !st.Valid() {
				return fmt.Errorf("a history holds an unknown state")
			}
		}
	}
	return nil
}
