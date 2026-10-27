package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// HostsPath serves the per-host view on the local listener.
const HostsPath = "/v1/hosts"

// HostsView is every configured host as the server sees it.
type HostsView struct {
	Hosts []HostState `json:"hosts"`
}

// HostState is one host's standing with the server.
//
// Silent is the verdict the page and the host.silent alert share, so doctor
// names exactly the hosts the notifiers were told about. Reported is false
// when nothing arrived since the server started.
type HostState struct {
	Host       string    `json:"host"`
	Silent     bool      `json:"silent"`
	Reported   bool      `json:"reported"`
	LastReport time.Time `json:"last_report,omitzero"`
}

// Hosts reports each configured host, sorted by name.
func (s *Server) Hosts() HostsView {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	view := HostsView{Hosts: make([]HostState, 0, len(s.hosts))}
	for _, host := range s.hosts {
		at, reported := s.lastHeard(host)
		st := HostState{Host: host, Silent: s.silentAt(host, now), Reported: reported}
		if reported {
			st.LastReport = at
		}
		view.Hosts = append(view.Hosts, st)
	}
	return view
}

// LocalHandler serves what only the operator may see: host names, which the
// public page never carries. It is bound to loopback alone and Caddy never
// routes to it, so reading it takes a shell on the status host, which is how
// `pilot doctor` reaches it.
func (s *Server) LocalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+HostsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		_ = json.NewEncoder(w).Encode(s.Hosts())
	})
	return mux
}
