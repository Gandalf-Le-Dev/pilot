package statuspage

import (
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
)

func TestWorse(t *testing.T) {
	order := []State{Up, Unknown, Degraded, Down}
	for i, a := range order {
		for j, b := range order {
			want := order[max(i, j)]
			if got := Worse(a, b); got != want {
				t.Errorf("Worse(%s, %s) = %s, want %s", a, b, got, want)
			}
		}
	}
	if State("on fire").Valid() || State("").Valid() {
		t.Error("only the four states are valid")
	}
}

func TestBucketStartAlignsToTheEpoch(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 7, 31, 0, time.UTC)
	if got := time.Unix(BucketStart(at), 0).UTC(); !got.Equal(time.Date(2026, 10, 5, 12, 5, 0, 0, time.UTC)) {
		t.Errorf("BucketStart = %s", got)
	}
}

// The config's floor for silent_after is written as a duration, not derived
// from the report interval, because config sits below this package. This
// keeps the two from drifting apart.
func TestMinSilentAfterCoversTwoReports(t *testing.T) {
	if got := config.MinSilentAfter.Duration(); got < 2*ReportInterval {
		t.Errorf("config.MinSilentAfter = %s, but one late report at a %s interval would fire it", got, ReportInterval)
	}
	if got := config.DefaultSilentAfter; got < config.MinSilentAfter {
		t.Errorf("the default silent_after %s is below the minimum", got)
	}
}
