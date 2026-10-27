package journal

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestHandlerPrefixesJournalPriority(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, "pilotd", true))

	log.Info("listening", "socket", "/run/pilot/agent.sock")
	log.Warn("skipping alert", "when", "nope > 1")
	log.Error("could not record state", "service", "web")

	want := []string{
		`<6>pilotd: level=INFO msg=listening socket=/run/pilot/agent.sock`,
		`<4>pilotd: level=WARN msg="skipping alert" when="nope > 1"`,
		`<3>pilotd: level=ERROR msg="could not record state" service=web`,
	}
	if got := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n"); !slices.Equal(got, want) {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPriorityFollowsSyslogLevels(t *testing.T) {
	tests := map[slog.Level]int{
		slog.LevelDebug:     7,
		slog.LevelInfo - 1:  7,
		slog.LevelInfo:      6,
		slog.LevelInfo + 2:  6,
		slog.LevelWarn:      4,
		slog.LevelError:     3,
		slog.LevelError + 4: 3,
	}
	for level, want := range tests {
		if got := priority(level); got != want {
			t.Errorf("priority(%s) = %d, want %d", level, got, want)
		}
	}
}

// Run by hand, a `<4>` in front of every line is noise nothing will strip.
func TestHandlerOutsideJournalHasNoPriority(t *testing.T) {
	var buf bytes.Buffer
	slog.New(NewHandler(&buf, "pilotd server", false)).Warn("cannot bind", "addr", "100.64.0.1:7070")

	if got, want := buf.String(), "pilotd server: level=WARN msg=\"cannot bind\" addr=100.64.0.1:7070\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A newline inside a value would start a journal entry with no priority and no
// attribution, so the whole record must stay on one line.
func TestHandlerKeepsMultilineValuesOnOneLine(t *testing.T) {
	var buf bytes.Buffer
	slog.New(NewHandler(&buf, "pilotd", true)).Warn("skipping", "err", errors.New("line one\nline two"))

	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Errorf("wrote %d lines, want 1: %q", n, buf.String())
	}
}

// Anything a record carries can come from outside, a service name or a
// remote error, so none of it may forge an entry at another priority.
func TestHandlerResistsLineInjection(t *testing.T) {
	const forged = "\n<0>pilotd: forged"
	tests := map[string]func(*slog.Logger){
		"message":    func(l *slog.Logger) { l.Warn("start" + forged) },
		"value":      func(l *slog.Logger) { l.Warn("start", "k", "v"+forged) },
		"key":        func(l *slog.Logger) { l.Warn("start", "k"+forged, "v") },
		"group":      func(l *slog.Logger) { l.WithGroup("g"+forged).Warn("start", "k", "v") },
		"attr group": func(l *slog.Logger) { l.Warn("start", slog.Group("g"+forged, "k", "v")) },
	}
	for name, log := range tests {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			log(slog.New(NewHandler(&buf, "pilotd", true)))

			out := buf.String()
			if strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "<4>pilotd: ") {
				t.Errorf("want one line led by <4>pilotd: , got %q", out)
			}
		})
	}
}

func TestHandlerKeepsAttrsAndGroups(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, "pilotd", true)).With("service", "web").WithGroup("probe")
	log.Error("failed", "status", 502)

	if got, want := buf.String(), "<3>pilotd: level=ERROR msg=failed service=web probe.status=502\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Loggers derived with With share one writer, so concurrent records at
// different levels must each keep their own priority.
func TestHandlerPriorityHoldsUnderConcurrency(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(NewHandler(&buf, "pilotd", true))
	warn, fail := base.With("k", "w"), base.With("k", "e")

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { warn.Warn("w") })
		wg.Go(func() { fail.Error("e") })
	}
	wg.Wait()

	for line := range strings.Lines(buf.String()) {
		ok := strings.HasPrefix(line, "<4>pilotd: level=WARN") || strings.HasPrefix(line, "<3>pilotd: level=ERROR")
		if !ok {
			t.Fatalf("line with a mismatched priority: %q", line)
		}
	}
}

func TestIsStream(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "stream"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	stream := fmt.Sprintf("%d:%d", st.Dev, st.Ino)

	tests := map[string]struct {
		env  string
		want bool
	}{
		"this file":      {stream, true},
		"unset":          {"", false},
		"another inode":  {fmt.Sprintf("%d:%d", st.Dev, st.Ino+1), false},
		"another device": {fmt.Sprintf("%d:%d", uint64(st.Dev)+1, st.Ino), false},
		"malformed":      {"journal", false},
		"not numbers":    {"a:b", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isStream(tt.env, f); got != tt.want {
				t.Errorf("isStream(%q) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

// A command's stderr can be swapped for a buffer, which is never the journal
// even when the process's own stderr is.
func TestIsStreamNeedsAFile(t *testing.T) {
	fi, err := os.Stderr.Stat()
	if err != nil {
		t.Skip("stderr cannot be stat'ed:", err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	stream := fmt.Sprintf("%d:%d", st.Dev, st.Ino)

	if !isStream(stream, os.Stderr) {
		t.Fatal("stderr does not match its own device and inode")
	}
	if isStream(stream, &bytes.Buffer{}) {
		t.Error("a buffer matched stderr's stream")
	}
}

// The runtime prints a panic with no priority, so the log must carry it at
// error level, with the stack, before the panic goes on and kills the process.
func TestLogPanicLogsAtErrorAndPanicsOn(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, "pilotd", true))

	defer func() {
		if v := recover(); v != "boom" {
			t.Errorf("recovered %v, want the panic to go on as boom", v)
		}
		out := buf.String()
		if !strings.HasPrefix(out, `<3>pilotd: level=ERROR msg="exiting on panic" panic=boom stack=`) {
			t.Errorf("got %q, want the panic logged at error priority", out)
		}
		if !strings.Contains(out, "TestLogPanicLogsAtErrorAndPanicsOn") {
			t.Errorf("got %q, want the stack of the panicking goroutine", out)
		}
	}()
	func() {
		defer LogPanic(log)
		panic("boom")
	}()
}
