package runtime

import (
	"context"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport"
)

func target(t *testing.T, releaseID string) *Target {
	t.Helper()
	return &Target{
		Service: &config.Service{Name: "api", Runtime: config.RuntimeCompose},
		Host:    &config.Host{Name: "web-1", Address: "web1.example.com"},
		Layout:  release.NewLayout(""),
		Release: releaseID,
	}
}

// `mv -T` is the whole reason this is safe. Without it, moving a symlink onto
// an existing symlink-to-a-directory puts the new link *inside* that directory
// and leaves `current` pointing at the old release.
func TestSwapScriptUsesAtomicRename(t *testing.T) {
	got := SwapScript(target(t, "0042-9f3ac1b"))

	if !strings.Contains(got, "mv -Tf") {
		t.Errorf("swap must use mv -T or it will nest the symlink:\n%s", got)
	}
	if !strings.Contains(got, "ln -sfn releases/0042-9f3ac1b") {
		t.Errorf("symlink target should be relative to the service directory:\n%s", got)
	}
	if !strings.Contains(got, "current.tmp") {
		t.Errorf("swap should go through a temporary link:\n%s", got)
	}
	// Refuses to clobber a real directory left where `current` should be.
	if !strings.Contains(got, "! -L") {
		t.Errorf("swap should refuse when current is not a symlink:\n%s", got)
	}
	if !strings.Contains(got, "test -d /opt/pilot/services/api/releases/0042-9f3ac1b") {
		t.Errorf("swap should verify the release is staged:\n%s", got)
	}

	// The pending link is created before it is renamed, never after.
	lnAt := strings.Index(got, "ln -sfn")
	mvAt := strings.Index(got, "mv -Tf")
	if lnAt < 0 || mvAt < 0 || lnAt > mvAt {
		t.Errorf("link must be created before the rename:\n%s", got)
	}
}

func TestTargetPaths(t *testing.T) {
	tg := target(t, "0042-9f3ac1b")
	tests := []struct{ got, want string }{
		{tg.ServiceDir(), "/opt/pilot/services/api"},
		{tg.ReleaseDir(), "/opt/pilot/services/api/releases/0042-9f3ac1b"},
		{tg.CurrentDir(), "/opt/pilot/services/api/current"},
		{tg.Label(), "api on web-1"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// fakeExec records WriteFile calls and satisfies the rest of the Executor
// interface with inert stubs.
type fakeExec struct {
	writes map[string][]byte
}

func (f *fakeExec) Run(context.Context, string) (transport.Result, error) {
	return transport.Result{}, nil
}
func (f *fakeExec) RunScript(context.Context, string) (transport.Result, error) {
	return transport.Result{}, nil
}
func (f *fakeExec) Stream(context.Context, string, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (f *fakeExec) RunInput(context.Context, string, []byte) (transport.Result, error) {
	return transport.Result{}, nil
}
func (f *fakeExec) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (f *fakeExec) WriteFile(_ context.Context, p string, data []byte, _ string) error {
	f.writes[p] = data
	return nil
}
func (f *fakeExec) Exists(context.Context, string) (bool, error)     { return false, nil }
func (f *fakeExec) HasCommand(context.Context, string) (bool, error) { return false, nil }
func (f *fakeExec) UploadDir(context.Context, string, string) error  { return nil }
func (f *fakeExec) MkdirAll(context.Context, string) error           { return nil }
func (f *fakeExec) RemoveAll(context.Context, string) error          { return nil }
func (f *fakeExec) Label() string                                    { return "fake" }

// An env-less compose service must still stage a .env: the compose runtime
// passes --env-file on every invocation, and docker compose errors on a
// missing file rather than treating it as empty. The first env-less compose
// service deployed for real failed staging on exactly this.
func TestStageCommonWritesEnvFile(t *testing.T) {
	cases := []struct {
		name    string
		runtime config.Runtime
		env     map[string]string
		want    bool
	}{
		{"compose without env", config.RuntimeCompose, nil, true},
		{"compose with env", config.RuntimeCompose, map[string]string{"A": "1"}, true},
		{"static without env", config.RuntimeStatic, nil, false},
		{"systemd without env", config.RuntimeSystemd, nil, false},
		{"systemd with env", config.RuntimeSystemd, map[string]string{"A": "1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExec{writes: map[string][]byte{}}
			tg := target(t, "0001-abc1234")
			tg.Service.Runtime = tc.runtime
			tg.Client = exec

			if err := StageCommon(context.Background(), tg, StageInput{Env: tc.env}); err != nil {
				t.Fatal(err)
			}

			body, wrote := exec.writes[path.Join(tg.ReleaseDir(), release.EnvFile)]
			if wrote != tc.want {
				t.Errorf("wrote .env = %v, want %v", wrote, tc.want)
			}
			if wrote && len(tc.env) == 0 && len(body) != 0 {
				t.Errorf("empty env should render an empty file, got %q", body)
			}
		})
	}
}

func TestRenderEnv(t *testing.T) {
	t.Run("sorted and stable", func(t *testing.T) {
		got, err := RenderEnv(map[string]string{"ZED": "3", "ALPHA": "1", "MID": "2"})
		if err != nil {
			t.Fatal(err)
		}
		want := "ALPHA=1\nMID=2\nZED=3\n"
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("quotes values that need it", func(t *testing.T) {
		got, err := RenderEnv(map[string]string{
			"PLAIN":  "simple",
			"URL":    "postgres://user:pw@db:5432/app",
			"SPACED": "two words",
			"HASH":   "a#b",
			"QUOTED": `say "hi"`,
			"EMPTY":  "",
		})
		if err != nil {
			t.Fatal(err)
		}
		s := string(got)
		for _, want := range []string{
			"PLAIN=simple\n",
			"SPACED=\"two words\"\n",
			"HASH=\"a#b\"\n",
			`QUOTED="say \"hi\""` + "\n",
			"EMPTY=\n",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %q in:\n%s", want, s)
			}
		}
	})

	// Neither compose nor systemd parses multi-line values consistently, so a
	// silently mangled secret is the worst outcome. Refuse instead.
	t.Run("rejects newlines", func(t *testing.T) {
		_, err := RenderEnv(map[string]string{"KEY": "-----BEGIN KEY-----\nabc\n"})
		if err == nil {
			t.Fatal("want an error")
		}
		if !strings.Contains(err.Error(), "newline") {
			t.Errorf("error should say why: %v", err)
		}
	})

	t.Run("rejects invalid names", func(t *testing.T) {
		for _, bad := range []string{"has-dash", "1LEADING", "has space", ""} {
			if _, err := RenderEnv(map[string]string{bad: "x"}); err == nil {
				t.Errorf("RenderEnv should reject the name %q", bad)
			}
		}
	})

	t.Run("byte-stable across calls", func(t *testing.T) {
		env := map[string]string{"A": "1", "B": "2", "C": "3", "D": "4"}
		first, err := RenderEnv(env)
		if err != nil {
			t.Fatal(err)
		}
		for range 20 {
			got, err := RenderEnv(env)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(first) {
				t.Fatalf("output is not stable:\n%q\n%q", first, got)
			}
		}
	})
}
