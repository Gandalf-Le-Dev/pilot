package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Gandalf-Le-Dev/pilot/internal/app"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/render"
)

func newTopCmd(g *globals) *cobra.Command {
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "top [service|@tag]",
		Short: "Watch the fleet, refreshing until interrupted",
		Long: "A repeating `status`, in a full-screen view that leaves your scrollback\n" +
			"intact. Useful while a deploy lands, or when waiting to see whether\n" +
			"something settles.\n\n" +
			"Press q or esc to quit, r to refresh immediately.\n\n" +
			"The rows are exactly what `pilot status` prints — the same renderer draws\n" +
			"both, so they cannot disagree about what is running.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			return runTop(cmd.Context(), g, selector, interval)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 3*time.Second, "how often to refresh")
	return cmd
}

func runTop(ctx context.Context, g *globals, selector string, interval time.Duration) error {
	// `top` redraws a full screen and its rows are exactly what `status`
	// produces, so there is no honest JSON form of it.
	if g.json {
		return fmt.Errorf("`top` is an interactive display and has no JSON form\n" +
			"poll `pilot status --json` instead; it returns the same rows")
	}

	// A full-screen view written to something that is not a screen produces a
	// file full of cursor movements. Say so rather than emit that.
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("`top` needs a terminal; its output is a live display, not a stream\n" +
			"use `pilot status` when output is piped or redirected")
	}

	a, err := g.load()
	if err != nil {
		return err
	}
	defer a.Close()

	services, err := selectServices(a, selector)
	if err != nil {
		return err
	}
	if interval < time.Second {
		interval = time.Second
	}

	// The TUI renders into a buffer that bubbletea writes to the terminal, so
	// the styler would judge that buffer colourless. stdout is the real
	// destination and it is a terminal, which is what was just checked.
	restore := render.ColourOverride(true)
	defer restore()

	m := topModel{app: a, services: services, interval: interval, loading: true}
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithAltScreen(), // leave the operator's scrollback untouched
	)

	if _, err := p.Run(); err != nil {
		// A cancelled context is the operator pressing ctrl-c, not a failure.
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return nil
}

// topModel is the live fleet view.
//
// The data is fetched in a command rather than in Update, so a slow or
// unreachable host cannot freeze the interface. The previous implementation
// gathered synchronously inside its redraw loop, which meant one wedged host
// stopped the whole display from responding — including to ctrl-c.
type topModel struct {
	app      *app.App
	services []*config.Service
	interval time.Duration

	rows     []render.StatusRow
	degraded map[string]string
	alerts   map[string][]string

	loading bool
	lastAt  time.Time
	err     error
	width   int
}

type refreshedMsg struct {
	rows     []render.StatusRow
	degraded map[string]string
	alerts   map[string][]string
	at       time.Time
}

type tickMsg time.Time

func (m topModel) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(m.interval))
}

// refresh gathers the fleet off the UI goroutine.
func (m topModel) refresh() tea.Cmd {
	return func() tea.Msg {
		// Bounded so a hung host cannot hold a refresh open past the next tick
		// and stack them up.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		views := gatherHosts(ctx, m.app, hostsFor(m.services))
		return refreshedMsg{
			rows:     buildRows(ctx, m.app, m.services, views),
			degraded: views.degraded(),
			alerts:   views.alerts(),
			at:       time.Now(),
		}
	}
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.loading = true
			return m, m.refresh()
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width

	case refreshedMsg:
		m.rows, m.degraded, m.alerts = msg.rows, msg.degraded, msg.alerts
		m.lastAt, m.loading = msg.at, false

	case tickMsg:
		m.loading = true
		return m, tea.Batch(m.refresh(), tick(m.interval))
	}
	return m, nil
}

func (m topModel) View() string {
	var b strings.Builder

	stamp := "—"
	if !m.lastAt.IsZero() {
		stamp = m.lastAt.Format("15:04:05")
	}
	status := ""
	if m.loading {
		status = "  refreshing…"
	}
	fmt.Fprintf(&b, "  pilot top — %s  (every %s)%s\n\n", stamp, m.interval, status)

	// The same renderer as `pilot status`, so the two can never disagree about
	// what is running — which is the whole reason this does not draw its own
	// table.
	if len(m.rows) == 0 && m.loading {
		b.WriteString("  gathering…\n")
	} else {
		render.Status(&b, m.rows)
		render.StatusFooter(&b, m.degraded, m.alerts)
	}

	b.WriteString("\n  q quit · r refresh\n")
	return b.String()
}
