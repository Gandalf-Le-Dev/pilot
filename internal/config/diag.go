package config

import (
	"fmt"
	"sort"
	"strings"
)

// Severity ranks a Diagnostic. Errors block a deploy; warnings never do.
type Severity int

const (
	SevError Severity = iota
	SevWarning
)

func (s Severity) String() string {
	switch s {
	case SevError:
		return "error"
	case SevWarning:
		return "warning"
	}
	return "unknown"
}

// Diagnostic is one problem found in the configuration. It carries enough
// context to be rendered by `pilot doctor` without further lookup: which file,
// which field, what's wrong, and — where we can offer one — how to fix it.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	File     string   `json:"file,omitempty"`
	Field    string   `json:"field,omitempty"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`
}

func (d Diagnostic) String() string {
	var b strings.Builder
	if d.File != "" {
		b.WriteString(d.File)
		b.WriteString(": ")
	}
	if d.Field != "" {
		b.WriteString(d.Field)
		b.WriteString(": ")
	}
	b.WriteString(d.Message)
	if d.Hint != "" {
		b.WriteString(" (")
		b.WriteString(d.Hint)
		b.WriteString(")")
	}
	return b.String()
}

// Diagnostics is an ordered collection of problems.
type Diagnostics []Diagnostic

func (ds *Diagnostics) Add(d Diagnostic) { *ds = append(*ds, d) }

func (ds *Diagnostics) Errorf(file, field, format string, args ...any) {
	ds.Add(Diagnostic{Severity: SevError, File: file, Field: field, Message: fmt.Sprintf(format, args...)})
}

func (ds *Diagnostics) Warnf(file, field, format string, args ...any) {
	ds.Add(Diagnostic{Severity: SevWarning, File: file, Field: field, Message: fmt.Sprintf(format, args...)})
}

// ErrorHint records an error along with a concrete suggested fix.
func (ds *Diagnostics) ErrorHint(file, field, msg, hint string) {
	ds.Add(Diagnostic{Severity: SevError, File: file, Field: field, Message: msg, Hint: hint})
}

// WarnHint records a warning along with a concrete suggested fix.
func (ds *Diagnostics) WarnHint(file, field, msg, hint string) {
	ds.Add(Diagnostic{Severity: SevWarning, File: file, Field: field, Message: msg, Hint: hint})
}

func (ds Diagnostics) Count(sev Severity) int {
	n := 0
	for _, d := range ds {
		if d.Severity == sev {
			n++
		}
	}
	return n
}

func (ds Diagnostics) Errors() int     { return ds.Count(SevError) }
func (ds Diagnostics) Warnings() int   { return ds.Count(SevWarning) }
func (ds Diagnostics) HasErrors() bool { return ds.Errors() > 0 }

// Err returns a single error summarising the diagnostics, or nil if none are
// errors. Useful at call sites that just need to abort.
func (ds Diagnostics) Err() error {
	if !ds.HasErrors() {
		return nil
	}
	var msgs []string
	for _, d := range ds {
		if d.Severity == SevError {
			msgs = append(msgs, d.String())
		}
	}
	return fmt.Errorf("invalid configuration:\n  %s", strings.Join(msgs, "\n  "))
}

// Sorted returns the diagnostics ordered by file, then severity (errors first),
// then field, so output is stable across runs regardless of map iteration.
func (ds Diagnostics) Sorted() Diagnostics {
	out := make(Diagnostics, len(ds))
	copy(out, ds)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Severity != out[j].Severity {
			return out[i].Severity < out[j].Severity
		}
		return out[i].Field < out[j].Field
	})
	return out
}
