package release

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	StateSchema = 1

	// MaxHistory caps the deploy log kept in state.json. Enough to answer
	// "what happened this week" without the file growing without bound.
	MaxHistory = 20
)

// Outcome is how a deploy attempt ended.
type Outcome string

const (
	OutcomeOK          Outcome = "ok"
	OutcomeFailed      Outcome = "failed"
	OutcomeRolledBack  Outcome = "rolled_back"
	OutcomeInterrupted Outcome = "interrupted"
)

// State is the mutable per-service record on the host: which release is live,
// what preceded it, and how recent deploys went.
//
// This is bookkeeping, not a source of truth. If state.json disappears, the
// `current` symlink still says what is running, and Pilot rebuilds from that.
type State struct {
	Schema   int    `json:"schema"`
	Service  string `json:"service"`
	Current  string `json:"current"`
	Previous string `json:"previous,omitempty"`

	// ActiveColor is which stack serves traffic under a blue-green rollout.
	// Recorded rather than inferred from the installed route, so that a
	// hand-edited route surfaces as drift instead of silently redirecting the
	// next deploy.
	ActiveColor string `json:"active_color,omitempty"`

	// History is newest-first.
	History []DeployRecord `json:"history,omitempty"`
}

// DeployRecord is one deploy attempt.
type DeployRecord struct {
	Release    string    `json:"release"`
	From       string    `json:"from,omitempty"`
	Host       string    `json:"host,omitempty"`
	By         string    `json:"by,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Outcome    Outcome   `json:"outcome"`
	Reason     string    `json:"reason,omitempty"`
}

func (r DeployRecord) Duration() time.Duration {
	if r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// NewState returns an empty state for a service.
func NewState(service string) *State {
	return &State{Schema: StateSchema, Service: service}
}

// Promote records that id is now the live release, shifting the old current
// into previous. Promoting the already-current release is a no-op, so a
// repeated activation doesn't destroy the rollback target.
func (s *State) Promote(id string) {
	if s.Current == id {
		return
	}
	s.Previous = s.Current
	s.Current = id
}

// Record prepends a deploy record, trimming history to MaxHistory.
func (s *State) Record(r DeployRecord) {
	s.History = append([]DeployRecord{r}, s.History...)
	if len(s.History) > MaxHistory {
		s.History = s.History[:MaxHistory]
	}
}

// LastDeploy returns the most recent record, or nil.
func (s *State) LastDeploy() *DeployRecord {
	if len(s.History) == 0 {
		return nil
	}
	return &s.History[0]
}

// RollbackTarget returns the release a rollback would activate.
func (s *State) RollbackTarget() (string, error) {
	if s.Previous == "" {
		return "", fmt.Errorf("service %q has no previous release to roll back to", s.Service)
	}
	return s.Previous, nil
}

// Protected returns the releases GC must never remove.
func (s *State) Protected() []string {
	var out []string
	if s.Current != "" {
		out = append(out, s.Current)
	}
	if s.Previous != "" {
		out = append(out, s.Previous)
	}
	return out
}

func MarshalState(s *State) ([]byte, error) {
	if s.Schema == 0 {
		s.Schema = StateSchema
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func UnmarshalState(b []byte) (*State, error) {
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("unreadable state: %w", err)
	}
	if s.Schema != StateSchema {
		return nil, fmt.Errorf("state schema %d, this build understands %d", s.Schema, StateSchema)
	}
	return &s, nil
}
