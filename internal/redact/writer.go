package redact

import (
	"bytes"
	"io"
	"strings"
)

// Placeholder replaces a credential in redacted output.
//
// Visible rather than blank, because a line that silently loses a field reads
// like the field was absent. `api_key=<redacted>` says a key was passed and
// declines to say which, which is what someone debugging a request needs.
const Placeholder = "<redacted>"

// maxLine caps how much is buffered while waiting for a newline.
//
// Redaction works a line at a time, since a credential split across two Write
// calls would otherwise slip through the middle. That means buffering, and a
// service emitting megabytes without a newline must not be able to exhaust
// memory on the machine reading its logs — so an over-long line is flushed
// as-is, redacted on what has arrived so far.
const maxLine = 1 << 20

// Text removes credentials from s, leaving enough behind to debug with.
//
// known maps names to values Pilot supplied to the service; pass nil when there
// is nothing to compare against. The layers run most-certain first, so an exact
// match is replaced before a pattern gets a chance to guess at the same text.
func Text(s string, known map[string]string) string {
	// Exact values Pilot itself provided. No guessing, so this goes first.
	for _, value := range known {
		if len(value) < minKnownLength || strings.Contains(value, "${") {
			continue
		}
		s = strings.ReplaceAll(s, value, Placeholder)
	}

	// Labelled parameters: keep the name and separator, replace the value.
	s = labelled.ReplaceAllStringFunc(s, func(match string) string {
		m := labelled.FindStringSubmatch(match)
		if placeholders[strings.ToLower(strings.Trim(m[3], `"'`))] {
			return match // already redacted; rewriting it would churn
		}
		return m[1] + m[2] + Placeholder
	})

	// Formats that can only be credentials.
	for _, f := range formats {
		s = f.re.ReplaceAllString(s, Placeholder)
	}
	return s
}

// Writer redacts a log stream on its way to w.
//
// Line-oriented, which is the point rather than an implementation detail: a
// credential straddling two Write calls would survive chunk-by-chunk redaction,
// and log output arrives in whatever sizes the transport chooses.
//
// It composes with the JSON log writer — `Logs → redact.Writer → json → stdout`
// — so the escaping and the cleaning are independent of each other.
type Writer struct {
	w     io.Writer
	known map[string]string
	buf   []byte
}

// NewWriter returns a Writer that redacts each line before passing it on.
func NewWriter(w io.Writer, known map[string]string) *Writer {
	return &Writer{w: w, known: known}
}

func (w *Writer) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)

	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		if err := w.emit(line + "\n"); err != nil {
			return len(p), err
		}
	}

	// No newline in sight and the buffer is growing without bound: flush what
	// there is rather than hold it forever.
	if len(w.buf) >= maxLine {
		line := string(w.buf)
		w.buf = nil
		if err := w.emit(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// Close flushes a trailing line that never got its newline.
//
// Without it the last line of a finite log is dropped — and it is exactly the
// line someone tailing a crashed service most wants to see.
func (w *Writer) Close() error {
	if len(w.buf) == 0 {
		return nil
	}
	line := string(w.buf)
	w.buf = nil
	return w.emit(line)
}

func (w *Writer) emit(s string) error {
	_, err := io.WriteString(w.w, Text(s, w.known))
	return err
}
