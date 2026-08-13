package static

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

func svc(static *config.StaticExpose) *config.Service {
	return &config.Service{
		Name:    "blog",
		Runtime: config.RuntimeStatic,
		Expose:  &config.Expose{Domains: []string{"blog.example.com"}, Static: static},
	}
}

// Carrying forward assets prevents a stale-asset 404, but doing it for the
// whole tree would mean deleted pages never disappear. The default is scoped;
// an explicit empty list turns it off.
func TestOverlayDirs(t *testing.T) {
	tests := []struct {
		name string
		svc  *config.Service
		want []string
	}{
		{"unset uses the default", svc(&config.StaticExpose{}), config.DefaultOverlayDirs},
		{"explicitly disabled", svc(&config.StaticExpose{Overlay: []string{}}), nil},
		{"custom directories", svc(&config.StaticExpose{Overlay: []string{"_next/static", "media"}}), []string{"_next/static", "media"}},
		{"no expose block at all", &config.Service{Name: "blog"}, config.DefaultOverlayDirs},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := overlayDirs(tc.svc)
			if len(got) != len(tc.want) {
				t.Fatalf("overlayDirs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("overlayDirs = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestIndexFile(t *testing.T) {
	tests := []struct {
		name string
		svc  *config.Service
		want string
	}{
		{"default", svc(&config.StaticExpose{}), config.DefaultIndex},
		{"custom", svc(&config.StaticExpose{Index: "home.html"}), "home.html"},
		{"no expose", &config.Service{Name: "blog"}, config.DefaultIndex},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := indexFile(tc.svc); got != tc.want {
				t.Errorf("indexFile = %q, want %q", got, tc.want)
			}
		})
	}
}

// A static site has no per-service log stream. Saying so plainly beats
// returning an empty stream that looks like "no output".
func TestLogsReportsThereAreNone(t *testing.T) {
	tg := &runtime.Target{
		Service: svc(&config.StaticExpose{}),
		Host:    &config.Host{Name: "web-1"},
		Layout:  release.NewLayout(""),
	}
	err := New().Logs(context.Background(), tg, runtime.LogOptions{}, io.Discard)
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, runtime.ErrNoLogs) {
		t.Errorf("error should wrap ErrNoLogs so callers can special-case it, got %v", err)
	}
}

// Taking a static site offline means removing its route, which the deploy
// pipeline owns — there is no process here to stop.
func TestDeactivateIsANoOp(t *testing.T) {
	tg := &runtime.Target{Service: svc(&config.StaticExpose{}), Layout: release.NewLayout("")}
	if err := New().Deactivate(context.Background(), tg); err != nil {
		t.Errorf("Deactivate should succeed trivially, got %v", err)
	}
}

func TestKind(t *testing.T) {
	if New().Kind() != config.RuntimeStatic {
		t.Error("wrong runtime kind")
	}
}

// fakeExec records the script Fingerprint runs, satisfying the rest of the
// Executor interface by embedding it — any other call would panic, which is
// the point: Fingerprint needs exactly one round-trip.
type fakeExec struct {
	transport.Executor
	script string
}

func (f *fakeExec) RunScript(ctx context.Context, body string) (transport.Result, error) {
	f.script = body
	return transport.Result{Stdout: "index.html 120\n"}, nil
}

// The agent caches each release's drift baseline inside the release directory
// itself, so the measurement must exclude it by name — otherwise recording
// the baseline changes the tree, and every static deploy drifts against
// itself one check-interval after activating.
func TestFingerprintExcludesItsOwnBaseline(t *testing.T) {
	fe := &fakeExec{}
	tg := &runtime.Target{
		Service: svc(&config.StaticExpose{}),
		Host:    &config.Host{Name: "web-1"},
		Layout:  release.NewLayout(""),
		Client:  fe,
	}

	if _, err := New().Fingerprint(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fe.script, "-not -name "+transport.Quote(release.FingerprintFile)) {
		t.Errorf("the find must ignore the baseline the agent writes into the tree:\n%s", fe.script)
	}
}
