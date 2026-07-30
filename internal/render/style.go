package render

import (
	"io"
	"os"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Styling is deliberately a property of the *writer*, not a global.
//
// Pilot's output is read by three different things and only one of them is a
// person: a terminal, a script parsing columns, and an AI agent with the skill
// installed. Escape codes are an improvement for the first and corruption for
// the other two — an agent that receives ANSI in its context has had its window
// filled with noise it cannot use, and a script's `awk` breaks silently.
//
// So colour is decided per call, from whether the destination is a terminal.
// A guard test asserts that a non-terminal writer produces no escape sequences
// at all, because this is exactly the kind of thing that regresses quietly and
// is discovered by somebody's pipeline months later.

// profile reports the colour capability of w.
//
// os.Stdout is checked directly rather than through the writer interface: a
// bytes.Buffer cannot be a terminal, and that is precisely the case tests and
// pipelines exercise.
func profile(w io.Writer) termenv.Profile {
	if f, ok := w.(*os.File); ok && termenv.NewOutput(f).Profile != termenv.Ascii {
		return termenv.NewOutput(f).Profile
	}
	return termenv.Ascii
}

// styler carries the palette for one destination. The Ascii profile makes every
// Render call a no-op returning its input unchanged, which is how non-terminal
// output stays byte-identical to what it was before any of this existed.
type styler struct {
	ok bool
	r  *lipgloss.Renderer
}

// newStyler builds a renderer bound to this destination.
//
// A dedicated renderer rather than lipgloss's global one, because the global
// decides its colour profile from os.Stdout at init. That is the wrong question
// when output is going somewhere else — and it silently strips colour in tests
// while claiming to apply it, which is how styling ends up as dead code nobody
// notices.
func newStyler(w io.Writer) styler {
	// SetColorProfile rather than termenv.WithProfile: the option sets the
	// output's profile, but Renderer.ColorProfile() ignores it and re-derives
	// from the environment unless the profile was set *explicitly*. Passing the
	// option alone produces a renderer that reports the right profile and
	// renders no colour at all — which looks exactly like styling that was never
	// wired up.
	if forced() {
		r := lipgloss.NewRenderer(w)
		r.SetColorProfile(termenv.TrueColor)
		return styler{ok: true, r: r}
	}

	p := profile(w)
	if p == termenv.Ascii {
		return styler{}
	}
	r := lipgloss.NewRenderer(w)
	r.SetColorProfile(p)
	return styler{ok: true, r: r}
}

// Colours are fixed, not adaptive, and that is a performance decision rather
// than an aesthetic one.
//
// lipgloss.AdaptiveColor resolves by asking the terminal for its background via
// an OSC 11 query and *blocking* for the reply. Terminals that do not answer —
// some emulators, some multiplexers, anything over a slow link — cost the full
// timeout, which measured here as `pilot status` going from 1.4s to 6.4s. A
// status command that pauses five seconds to decide on a shade of green is a
// worse outcome than a shade of green that is slightly off.
//
// These are ANSI-256 values picked to stay legible on both light and dark
// backgrounds. Nothing carries meaning by colour alone: every coloured string is
// already prefixed by a symbol or a word, so a monochrome terminal loses
// decoration rather than information.
var (
	colOK     = lipgloss.Color("71")  // green
	colWarn   = lipgloss.Color("179") // amber
	colFail   = lipgloss.Color("167") // red
	colMuted  = lipgloss.Color("245") // grey
	colHeader = lipgloss.Color("252") // near-white, bolded
)

// colour applies a foreground only when the destination can show it.
//
// Styles come from s.r, never from lipgloss.NewStyle(). The package-level
// constructor uses lipgloss's *global* renderer, whose profile is decided from
// os.Stdout at init — so a style built that way renders plain no matter what
// this styler has been told, and the whole thing looks like it was never wired
// up. That mistake was made here once already and cost an hour.
func (s styler) colour(c lipgloss.TerminalColor, text string) string {
	if !s.ok {
		return text
	}
	return s.r.NewStyle().Foreground(c).Render(text)
}

func (s styler) ok_(text string) string   { return s.colour(colOK, text) }
func (s styler) warn(text string) string  { return s.colour(colWarn, text) }
func (s styler) fail(text string) string  { return s.colour(colFail, text) }
func (s styler) muted(text string) string { return s.colour(colMuted, text) }

func (s styler) header(text string) string {
	if !s.ok {
		return text
	}
	return s.r.NewStyle().Foreground(colHeader).Bold(true).Render(text)
}

// forceColour lets a test exercise the styled path without a real terminal.
var forceColour struct {
	sync.Mutex
	on bool
}

// ForceColourForTest turns styling on regardless of the destination, and
// returns a function restoring the previous setting.
func ForceColourForTest(on bool) func() {
	forceColour.Lock()
	prev := forceColour.on
	forceColour.on = on
	forceColour.Unlock()
	return func() {
		forceColour.Lock()
		forceColour.on = prev
		forceColour.Unlock()
	}
}

func forced() bool {
	forceColour.Lock()
	defer forceColour.Unlock()
	return forceColour.on
}

// state colours a service's state cell.
//
// The mapping is deliberately coarse: running is fine, stopped or failed needs
// attention, anything else is unknown and therefore a question rather than an
// answer. `unknown` is warned rather than failed — Pilot could not observe the
// service, which is not the same as the service being down, and colouring it
// red would report an outage that may not exist.
func (s styler) state(name, text string) string {
	switch name {
	case "running":
		return s.ok_(text)
	case "stopped", "failed", "degraded":
		return s.fail(text)
	default:
		return s.warn(text)
	}
}
