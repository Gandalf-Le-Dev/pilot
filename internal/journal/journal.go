// Package journal formats pilotd's log for systemd's journal.
//
// Each record is one logfmt line, attributed to the process that wrote it and
// without a timestamp, since journald stamps every entry itself. Under systemd
// the line also starts with a `<N>` syslog priority, which journald strips and
// files as the entry's level (SyslogLevelPrefix=, on by default), so
// `journalctl -p warning` finds warnings.
package journal

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// NewHandler returns a handler writing to w, each line led by name. With
// priorities, each line also starts with its syslog priority.
func NewHandler(w io.Writer, name string, priorities bool) slog.Handler {
	out := &lineWriter{w: w, name: name, priorities: priorities}
	return &handler{
		inner: slog.NewTextHandler(out, &slog.HandlerOptions{ReplaceAttr: dropTime}),
		out:   out,
	}
}

// OnJournal reports whether w is the stream systemd connected to the journal.
// systemd sets JOURNAL_STREAM to that stream's device and inode, and comparing
// them with w's own tells a unit's stderr from a child that inherited the
// variable but not the stream. Only an *os.File can be the stream.
func OnJournal(w io.Writer) bool { return isStream(os.Getenv("JOURNAL_STREAM"), w) }

func isStream(stream string, w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	dev, ino, ok := strings.Cut(stream, ":")
	if !ok {
		return false
	}
	wantDev, err := strconv.ParseUint(dev, 10, 64)
	if err != nil {
		return false
	}
	wantIno, err := strconv.ParseUint(ino, 10, 64)
	if err != nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == wantDev && st.Ino == wantIno
}

// LogPanic, deferred at the top of a goroutine, logs a panic at error level
// before letting it go on. The runtime's own report reaches the journal with
// no priority, so `journalctl -p err` would miss it. A panic passes through
// only the deferred calls of its own goroutine, so every goroutine a daemon
// starts defers this, except http.Server.Serve: net/http recovers and logs
// its handlers' panics itself.
func LogPanic(log *slog.Logger) {
	if v := recover(); v != nil {
		log.Error("exiting on panic", "panic", v, "stack", string(debug.Stack()))
		panic(v)
	}
}

type handler struct {
	inner slog.Handler
	out   *lineWriter
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

// Handle relies on the text handler writing each record in a single Write,
// made while out's lock is held so the priority belongs to this record.
// Formatting runs under that lock too, so a value's LogValue, Error or String
// method must not log through this handler: it would deadlock.
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	h.out.mu.Lock()
	defer h.out.mu.Unlock()
	h.out.level = r.Level
	return h.inner.Handle(ctx, r)
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	return &handler{inner: h.inner.WithAttrs(as), out: h.out}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{inner: h.inner.WithGroup(name), out: h.out}
}

// lineWriter is shared by every handler derived from one NewHandler.
type lineWriter struct {
	mu         sync.Mutex
	w          io.Writer
	name       string
	priorities bool
	level      slog.Level
}

func (l *lineWriter) Write(p []byte) (int, error) {
	line := make([]byte, 0, len(l.name)+len(p)+5)
	if l.priorities {
		line = append(line, '<')
		line = strconv.AppendInt(line, int64(priority(l.level)), 10)
		line = append(line, '>')
	}
	line = append(line, l.name...)
	line = append(line, ": "...)
	line = append(line, p...)
	if _, err := l.w.Write(line); err != nil {
		return 0, err
	}
	return len(p), nil
}

// priority maps a slog level to syslog's: err, warning, info, debug. slog has
// no notice or crit, so levels between its own round down.
func priority(l slog.Level) int {
	switch {
	case l >= slog.LevelError:
		return 3
	case l >= slog.LevelWarn:
		return 4
	case l >= slog.LevelInfo:
		return 6
	default:
		return 7
	}
}

func dropTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}
