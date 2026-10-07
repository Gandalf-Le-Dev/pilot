// Package statuspage is the contract between agents and the status server,
// and the public page the server renders from it.
//
// Everything an agent sends is defined here, and nothing in it is free text.
// A report carries service names, enum states and timestamps; an error
// message, a release ID or a health-check URL has no field to travel in. That
// is the leak guard by construction: the server decodes strictly, so a field
// added on one side without the other is a rejected report, not a disclosure.
package statuspage

import "time"

const (
	// ReportInterval is how often an agent samples and reports.
	ReportInterval = 30 * time.Second

	// BucketWidth and HistoryBuckets make the history 24 hours of five-minute
	// buckets: fine enough to show a short outage, small enough to resend in
	// full every report, which is what lets the server keep nothing on disk.
	BucketWidth    = 5 * time.Minute
	HistoryBuckets = 288
)

// State is a service's condition as the public sees it.
type State string

const (
	Up       State = "up"
	Degraded State = "degraded"
	Down     State = "down"
	Unknown  State = "unknown"
)

// Valid reports whether s is one of the four states.
func (s State) Valid() bool { return severity(s) >= 0 }

func severity(s State) int {
	switch s {
	case Up:
		return 0
	case Unknown:
		return 1
	case Degraded:
		return 2
	case Down:
		return 3
	}
	return -1
}

// Worse returns the more serious of two states.
//
// Unknown ranks above up and below everything else: a bucket where one
// sample could not be taken is not a clean bucket, but it is not evidence of
// an outage either.
func Worse(a, b State) State {
	if severity(b) > severity(a) {
		return b
	}
	return a
}

// BucketStart returns the unix time of the bucket holding t. Buckets align to
// the epoch, so every agent's history lines up with every other's.
func BucketStart(t time.Time) int64 {
	u := t.Unix()
	width := int64(BucketWidth / time.Second)
	return u - u%width
}

// Report is one agent's snapshot, posted every ReportInterval.
type Report struct {
	Host     string          `json:"host"`
	Services []ServiceReport `json:"services"`
}

// ServiceReport is one service's current state and recent history.
type ServiceReport struct {
	Name    string  `json:"name"`
	State   State   `json:"state"`
	History History `json:"history"`
}

// History is the worst state seen in each five-minute bucket, oldest first.
//
// End is the start of the newest bucket, as unix seconds; the bucket before
// it starts BucketWidth earlier, and so on. A bucket with no sample, such as
// one where the agent was down, reads Unknown.
type History struct {
	End    int64   `json:"end"`
	States []State `json:"states"`
}
