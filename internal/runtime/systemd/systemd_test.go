package systemd

import (
	"strings"
	"testing"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
)

func oneshot(fresh time.Duration) *config.Unit {
	return &config.Unit{
		Name:  "plakar-backup.service",
		Kind:  config.UnitOneshot,
		Timer: "plakar-backup.timer",
		Fresh: config.Duration(fresh),
	}
}

// The properties a freshly installed timer reports before it has ever fired,
// copied verbatim from a real host. Result is "success" and ExecMainStatus is
// "0" on a unit that has never run: systemd initialises them that way.
//
// This is the whole reason classifyOneshot exists. A reasonable implementation
// reads Result, sees success, and reports a backup as healthy for as long as
// the machine stays up — without a single backup having been taken.
var neverRan = map[string]string{
	"ActiveState":            "inactive",
	"SubState":               "dead",
	"Result":                 "success",
	"ExecMainStatus":         "0",
	"ExecMainStartTimestamp": "",
	"ExecMainExitTimestamp":  "",
}

func TestClassifyOneshotNeverRanIsNotSuccess(t *testing.T) {
	v := classifyOneshot(oneshot(48*time.Hour), neverRan, time.Now())

	if v.state == runtime.StateRunning {
		t.Fatal("a unit that has never run must not report healthy — systemd sets Result=success before the first run")
	}
	if v.state != runtime.StateStopped {
		t.Errorf("state = %q, want %q", v.state, runtime.StateStopped)
	}
	if !strings.Contains(v.detail, "never run") {
		t.Errorf("detail = %q, want it to say the unit has never run", v.detail)
	}
}

func TestClassifyOneshot(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ranAt := func(ago time.Duration) string {
		return now.Add(-ago).Format(stampLayout)
	}

	tests := []struct {
		name  string
		unit  *config.Unit
		props map[string]string
		want  runtime.State
		// detail is a substring; empty means "don't care".
		detail string
	}{
		{
			name: "recent success is healthy",
			unit: oneshot(48 * time.Hour),
			props: map[string]string{
				"ActiveState": "inactive", "SubState": "dead", "Result": "success",
				"ExecMainStartTimestamp": ranAt(2 * time.Hour),
				"ExecMainExitTimestamp":  ranAt(2 * time.Hour),
			},
			want: runtime.StateRunning,
		},
		{
			// The failure this whole runtime exists to catch: a job that
			// worked, then silently stopped, and whose last result is still
			// "success" months later.
			name: "stale success is degraded",
			unit: oneshot(48 * time.Hour),
			props: map[string]string{
				"ActiveState": "inactive", "SubState": "dead", "Result": "success",
				"ExecMainStartTimestamp": ranAt(60 * 24 * time.Hour),
				"ExecMainExitTimestamp":  ranAt(60 * 24 * time.Hour),
			},
			want:   runtime.StateDegraded,
			detail: "last succeeded 60d ago, past the 48h freshness bound",
		},
		{
			// Without a bound there is nothing to compare against, so the last
			// success stands forever. Validation warns about this; the runtime
			// must not invent a default and silently disagree with the config.
			name: "stale success with no freshness bound stays healthy",
			unit: oneshot(0),
			props: map[string]string{
				"ActiveState": "inactive", "SubState": "dead", "Result": "success",
				"ExecMainStartTimestamp": ranAt(60 * 24 * time.Hour),
				"ExecMainExitTimestamp":  ranAt(60 * 24 * time.Hour),
			},
			want: runtime.StateRunning,
		},
		{
			name: "failed run",
			unit: oneshot(48 * time.Hour),
			props: map[string]string{
				"ActiveState": "failed", "SubState": "failed", "Result": "exit-code",
				"ExecMainStatus":         "1",
				"ExecMainStartTimestamp": ranAt(time.Hour),
				"ExecMainExitTimestamp":  ranAt(time.Hour),
			},
			want:   runtime.StateFailed,
			detail: "result=exit-code",
		},
		{
			name: "mid-run is not judged",
			unit: oneshot(48 * time.Hour),
			props: map[string]string{
				"ActiveState": "activating", "SubState": "start", "Result": "success",
				"ExecMainStartTimestamp": ranAt(time.Minute),
				"ExecMainExitTimestamp":  "",
			},
			want:   runtime.StateRunning,
			detail: "running now",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := classifyOneshot(tc.unit, tc.props, now)
			if v.state != tc.want {
				t.Errorf("state = %q, want %q (detail %q)", v.state, tc.want, v.detail)
			}
			if tc.detail != "" && !strings.Contains(v.detail, tc.detail) {
				t.Errorf("detail = %q, want it to contain %q", v.detail, tc.detail)
			}
		})
	}
}

func TestObserveService(t *testing.T) {
	u := &config.Unit{Name: "hopboxd.service"}

	tests := []struct {
		active string
		want   runtime.State
	}{
		{"active", runtime.StateRunning},
		{"activating", runtime.StateDegraded},
		{"deactivating", runtime.StateDegraded},
		{"failed", runtime.StateFailed},
		{"inactive", runtime.StateStopped},
		{"something-new", runtime.StateUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.active, func(t *testing.T) {
			obs, err := observeService(u, map[string]string{
				"ActiveState": tc.active, "SubState": "running", "Result": "success",
				"NRestarts":            "3",
				"ActiveEnterTimestamp": "Thu 2026-07-30 09:13:59 UTC",
			}, runtime.Observation{State: runtime.StateUnknown})
			if err != nil {
				t.Fatal(err)
			}
			if obs.State != tc.want {
				t.Errorf("ActiveState=%q gave %q, want %q", tc.active, obs.State, tc.want)
			}
			if len(obs.Instances) != 1 || obs.Instances[0].Restarts != 3 {
				t.Errorf("instance not populated: %+v", obs.Instances)
			}
		})
	}
}

// systemd's own timestamp format, as emitted by `systemctl show` on the hosts
// this runs against.
func TestParseStamp(t *testing.T) {
	got := parseStamp("Thu 2026-07-30 09:13:59 UTC")
	want := time.Date(2026, 7, 30, 9, 13, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseStamp = %v, want %v", got, want)
	}

	// systemd writes an empty value for "never", and that must not become the
	// Unix epoch — a job that never ran would then look 56 years stale rather
	// than never run at all.
	if ts := parseStamp(""); !ts.IsZero() {
		t.Errorf("empty stamp = %v, want the zero time", ts)
	}
	if ts := parseStamp("not a date"); !ts.IsZero() {
		t.Errorf("unparseable stamp = %v, want the zero time", ts)
	}
}

// Values routinely contain `=` — ExecStart and Environment always do. Splitting
// on every separator instead of the first would truncate them silently.
func TestParseShowSplitsOnFirstEquals(t *testing.T) {
	props := parseShow("ActiveState=active\nExecStart={ path=/usr/bin/x ; argv[]=/usr/bin/x --flag=1 }\nEmpty=\n")

	if props["ActiveState"] != "active" {
		t.Errorf("ActiveState = %q", props["ActiveState"])
	}
	if !strings.Contains(props["ExecStart"], "--flag=1") {
		t.Errorf("ExecStart was truncated: %q", props["ExecStart"])
	}
	if _, ok := props["Empty"]; !ok {
		t.Error("an empty value should still be present — it is how systemd says `never`")
	}
}

func TestRestartTarget(t *testing.T) {
	tests := []struct {
		name string
		unit *config.Unit
		want string
	}{
		{"a daemon restarts itself", &config.Unit{Name: "hopboxd.service"}, "hopboxd.service"},
		// Restarting a oneshot's service unit runs the job immediately, which
		// a deploy never asked for and which for a backup could mean hours.
		{"a oneshot restarts its timer", oneshot(0), "plakar-backup.timer"},
		{"a oneshot with no timer restarts nothing",
			&config.Unit{Name: "x.service", Kind: config.UnitOneshot}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.unit.RestartTarget(); got != tc.want {
				t.Errorf("RestartTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

// Links must resolve through `current`, never through a release directory.
// That is what makes rollback free — the swap alone repoints them — and a link
// baked to a release id would leave the binary and the release disagreeing
// after any rollback.
func TestLinkScriptPointsThroughCurrent(t *testing.T) {
	u := &config.Unit{
		Name: "hopboxd.service",
		Links: map[string]string{
			"/usr/local/bin/hopboxd":    "hopboxd",
			"/usr/local/bin/hopbox-mcp": "hopbox-mcp",
		},
	}
	script := linkScript(u, "/opt/pilot/services/hopboxd/current")

	if !strings.Contains(script, "/opt/pilot/services/hopboxd/current/hopboxd") {
		t.Errorf("link does not resolve through current:\n%s", script)
	}
	if strings.Contains(script, "/releases/") {
		t.Errorf("link is baked to a release directory, so rollback would not repoint it:\n%s", script)
	}
	if !strings.Contains(script, "will not replace it") {
		t.Errorf("script must refuse to clobber a real file:\n%s", script)
	}
	// Refusing is only half of it. This fires on the most ordinary case there
	// is — adopting a service whose binary someone installed by hand — so the
	// message has to say what to do next.
	if !strings.Contains(script, ".pre-pilot") {
		t.Errorf("refusal must name the remedy, not just the problem:\n%s", script)
	}
	if !strings.Contains(script, "mv -Tf") {
		t.Errorf("relink must be atomic, not a bare ln -sfn:\n%s", script)
	}

	// Sorted, so the script is byte-stable and a diff of two deploys shows
	// only what actually changed.
	if strings.Index(script, "hopbox-mcp") > strings.Index(script, "bin/hopboxd") {
		t.Error("links should be emitted in sorted order")
	}
}
