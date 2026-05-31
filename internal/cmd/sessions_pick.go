package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsPickCmd = &cobra.Command{
	Use:   "pick",
	Short: "Interactively pick a session",
	Long: `Show an interactive list of sessions and open the selected one.

Arrow keys navigate, Enter selects, q or Ctrl+C exits without selection.

By default, runs "crush sessions last <id>" on the selected session.
Use --tail to run "crush sessions tail <id> --follow" instead.

Only the 15 most recently active sessions are shown in the picker —
older ones are hidden and a "(+N not shown)" footer reports how many.
Run "crush sessions list" to see every session.`,
	Example: `
# Pick a session and show last 10 messages
crush sessions pick

# Pick a session and tail it live
crush sessions pick --tail
  `,
	RunE: sessionsPickCmdRun,
}

func sessionsPickCmdRun(cmd *cobra.Command, args []string) error {
	tail, _ := cmd.Flags().GetBool("tail")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sessions, err := a.Sessions.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	// Filter out internal child sessions.
	visible := sessions[:0]
	for _, s := range sessions {
		if s.ParentSessionID != "" {
			continue
		}
		visible = append(visible, s)
	}
	sessions = visible

	if len(sessions) == 0 {
		fmt.Fprintln(os.Stderr, "(no sessions)")
		return nil
	}

	items := make([]sessionItem, len(sessions))
	now := time.Now()
	for i, s := range sessions {
		items[i] = sessionItem{
			id:      s.ID,
			hash:    short(session.HashID(s.ID)),
			title:   truncate(s.Title, 40),
			updated: time.Unix(s.UpdatedAt, 0).Format("2006-01-02 15:04"),
			cost:    s.Cost,
			ago:     formatAge(now.Sub(time.Unix(s.UpdatedAt, 0))),
		}
	}
	items, hidden := trimSessionItems(items, pickerMaxItems)

	m := pickerModel{
		items:   items,
		hidden:  hidden,
		cursor:  0,
		tail:    tail,
		binary:  os.Args[0],
		quit:    false,
		swapped: false,
	}

	p := tea.NewProgram(&m)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("failed to run picker: %w", err)
	}

	if m.quit || m.selected == "" {
		return nil
	}

	fmt.Fprintf(os.Stderr, "selected: %s\n", m.selected)

	var cmdArgs []string
	if tail {
		cmdArgs = []string{"sessions", "tail", m.selected, "--follow"}
	} else {
		cmdArgs = []string{"sessions", "last", m.selected}
	}

	subCmd := exec.CommandContext(context.Background(), m.binary, cmdArgs...)
	subCmd.Stdin = os.Stdin
	subCmd.Stdout = os.Stdout
	subCmd.Stderr = os.Stderr
	return subCmd.Run()
}

// pickerMaxItems caps how many session rows the interactive picker
// shows. Sessions come back from the DB ordered by updated_at DESC,
// so this is the N most-recently-active sessions. Older ones are
// reachable by id via `sessions list` / `sessions show`. The cap keeps
// the picker fit-on-screen on small terminals and avoids a wall of
// rows when the DB has hundreds of entries.
const pickerMaxItems = 15

// trimSessionItems caps items to the first max entries and returns how
// many were dropped, so the picker view can show "(+N more not shown)".
// max <= 0 disables the cap. A nil / empty input is returned as-is.
func trimSessionItems(items []sessionItem, max int) ([]sessionItem, int) {
	if max <= 0 || len(items) <= max {
		return items, 0
	}
	return items[:max], len(items) - max
}

type sessionItem struct {
	id      string
	hash    string
	title   string
	updated string
	cost    float64
	ago     string
}

type pickerModel struct {
	items    []sessionItem
	hidden   int // sessions excluded by pickerMaxItems; shown as a footer
	cursor   int
	selected string
	tail     bool
	binary   string
	quit     bool
	swapped  bool
	width    int
	height   int
}

func (m *pickerModel) Init() tea.Cmd {
	return nil
}

func (m *pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.InterruptMsg:
		m.quit = true
		return m, tea.Quit
	case tea.KeyPressMsg:
		switch msg.Code {
		case tea.KeyUp, rune('k'):
			if m.cursor > 0 {
				m.cursor--
			}
		case tea.KeyDown, rune('j'):
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case tea.KeyEnter:
			m.selected = m.items[m.cursor].id
			return m, tea.Quit
		case rune('q'), tea.KeyEscape:
			m.quit = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *pickerModel) View() tea.View {
	var b strings.Builder

	fmt.Fprintf(&b, "  Sessions (↑/↓ navigate, Enter select, q quit)\n\n")

	// Column header. Two leading spaces match the cursor column width so
	// columns align under their headers regardless of which row is
	// selected. ID column is 32 chars wide — covers UUIDs (36 chars,
	// truncated with ellipsis) and slug-style ids set via `--session <slug>`.
	fmt.Fprintf(&b, "   %-8s  %-32s  %-30s  %-16s  %-7s  %s\n",
		"HASH", "ID", "TITLE", "UPDATED", "COST", "AGE")

	for i, item := range m.items {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		fmt.Fprintf(&b, "%s %-8s  %-32s  %-30s  %-16s  $%-6.4f  %s\n",
			cursor,
			item.hash,
			truncate(item.id, 32),
			truncate(item.title, 30),
			item.updated,
			item.cost,
			item.ago,
		)
	}
	if m.hidden > 0 {
		fmt.Fprintf(&b, "\n  (+%d older sessions not shown — use `sessions list` to see all)\n", m.hidden)
	}

	return tea.NewView(b.String())
}

func init() {
	sessionsPickCmd.Flags().Bool("tail", false, "Tail --follow the selected session instead of showing last N messages")
}
