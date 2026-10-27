package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/local"
)

// unreachable is a host that answers no command.
type unreachable struct{ *fakeExec }

func (unreachable) Run(context.Context, string) (transport.Result, error) {
	return transport.Result{}, errors.New("ssh: connection reset")
}

func stateTarget(t *testing.T, blueGreen bool) (*Target, string) {
	t.Helper()
	tg := &Target{
		Service: &config.Service{Name: "api", Runtime: config.RuntimeCompose},
		Host:    &config.Host{Name: "web-1"},
		Layout:  release.NewLayout(t.TempDir()),
		Client:  local.New("web-1"),
	}
	if blueGreen {
		tg.Service.Rollout = &config.Rollout{Strategy: config.StrategyBlueGreen, Service: "web", Ports: []int{18080, 18081}}
	}
	file := tg.Layout.State("api")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	return tg, file
}

// A missing state is a service never deployed; one that cannot be parsed is
// an error that names the file and how to recover.
func TestReadStateTellsMissingFromUnreadable(t *testing.T) {
	tg, file := stateTarget(t, false)
	if st, err := ReadState(t.Context(), tg); err != nil || st.Current != "" {
		t.Fatalf("missing state: %+v, %v", st, err)
	}

	if err := os.WriteFile(file, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadState(t.Context(), tg)
	var se *release.StateError
	if !errors.As(err, &se) || se.Path != file {
		t.Fatalf("want a StateError naming %s, got %v", file, err)
	}
	for _, want := range []string{file, "move it aside", "rebuilds `current` from the symlink", "active_color"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}

	st, unreadable, err := ReadStateOrBlank(t.Context(), tg)
	if err != nil || unreadable == nil || st == nil || st.Current != "" {
		t.Errorf("ReadStateOrBlank = %+v, %v, %v; want a blank state and the file named", st, unreadable, err)
	}
}

// A finished job records into a state that cannot be read only where a blank
// one says nothing false.
func TestStateToRecord(t *testing.T) {
	var warned []string
	warn := func(msg string, _ ...any) { warned = append(warned, msg) }

	for _, tc := range []struct {
		name      string
		blueGreen bool
		want      bool
	}{
		{"replaced outside blue-green", false, true},
		{"left alone under blue-green", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warned = nil
			tg, file := stateTarget(t, tc.blueGreen)
			if err := os.WriteFile(file, []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, ok := StateToRecord(t.Context(), tg, warn); ok != tc.want {
				t.Errorf("ok = %v, want %v", ok, tc.want)
			}
			if len(warned) != 1 {
				t.Errorf("warned %v, want once", warned)
			}
		})
	}

	t.Run("a state that could not be reached is never overwritten", func(t *testing.T) {
		warned = nil
		tg, _ := stateTarget(t, false)
		tg.Client = unreachable{&fakeExec{}}
		if _, ok := StateToRecord(t.Context(), tg, warn); ok || len(warned) != 1 {
			t.Errorf("ok = %v, warned %v; want no write and one warning", ok, warned)
		}
	})
}
