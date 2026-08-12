package doctor

import (
	"strings"
	"testing"
)

const recorded = `[Unit]
Description=plakar off-site backup
After=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/plakar-backup
# A backup that hangs forever looks identical to one still working.
TimeoutStartSec=2h
`

// `systemctl cat` frames each source file with a `# /path` line and appends
// drop-ins. That framing is systemd's, not the operator's, and comparing it
// would report drift on every host whose unit sits somewhere slightly
// different.
func TestDiffUnitsIgnoresSystemctlFraming(t *testing.T) {
	got := "# /etc/systemd/system/plakar-backup.service\n" + recorded

	added, removed := diffUnits([]byte(recorded), got)
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("identical units reported a difference:\n+%v\n-%v", added, removed)
	}
}

// Reordering keys inside a section changes nothing about behaviour. A check
// that cried drift over it would be switched off within a week.
func TestDiffUnitsIgnoresReordering(t *testing.T) {
	reordered := "[Service]\nExecStart=/usr/local/bin/plakar-backup\nType=oneshot\n" +
		"# A backup that hangs forever looks identical to one still working.\nTimeoutStartSec=2h\n" +
		"[Unit]\nDescription=plakar off-site backup\nAfter=network-online.target\n"

	added, removed := diffUnits([]byte(recorded), reordered)
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("reordering reported a difference:\n+%v\n-%v", added, removed)
	}
}

func TestDiffUnitsFindsRealChanges(t *testing.T) {
	// Someone raised the timeout on the host and never wrote it down.
	onHost := strings.Replace(recorded, "TimeoutStartSec=2h", "TimeoutStartSec=6h", 1)

	added, removed := diffUnits([]byte(recorded), onHost)
	if len(added) != 1 || added[0] != "TimeoutStartSec=6h" {
		t.Errorf("added = %v, want the host's new value", added)
	}
	if len(removed) != 1 || removed[0] != "TimeoutStartSec=2h" {
		t.Errorf("removed = %v, want the recorded value", removed)
	}
}

// A drop-in overrides the unit without touching the file, which is exactly
// where an out-of-band change tends to hide.
func TestDiffUnitsSeesDropIns(t *testing.T) {
	got := "# /etc/systemd/system/plakar-backup.service\n" + recorded +
		"\n# /etc/systemd/system/plakar-backup.service.d/override.conf\n[Service]\nNice=-5\n"

	added, _ := diffUnits([]byte(recorded), got)
	if len(added) != 1 || added[0] != "Nice=-5" {
		t.Errorf("added = %v, want the drop-in's line", added)
	}
}

// A deleted comment is a real loss: the comment explaining why a timeout is
// what it is, is the first thing to go and the thing you most want back.
func TestDiffUnitsNoticesDeletedComments(t *testing.T) {
	stripped := strings.Replace(recorded,
		"# A backup that hangs forever looks identical to one still working.\n", "", 1)

	_, removed := diffUnits([]byte(recorded), stripped)
	if len(removed) != 1 || !strings.HasPrefix(removed[0], "# A backup") {
		t.Errorf("removed = %v, want the deleted comment", removed)
	}
}

// The rendered diff must stay bounded; one reordered file should not fill a
// terminal with text nobody reads.
func TestUnitDiffIsBounded(t *testing.T) {
	many := make([]string, 20)
	for i := range many {
		many[i] = "Line=" + strings.Repeat("x", i+1)
	}
	out := unitDiff(many, nil)
	if !strings.Contains(out, "and 14 more") {
		t.Errorf("diff is not bounded:\n%s", out)
	}
}
