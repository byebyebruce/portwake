// Package tui is the optional bubbletea terminal dashboard.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"dcpentry/internal/config"
	"dcpentry/internal/events"
	"dcpentry/internal/project"
)

// Backend is what the dashboard needs from the application.
type Backend interface {
	Projects() []config.Project
	Snapshots() []project.Snapshot
	StartProject(name string) error
	StopProject(name string) error
	Bus() *events.Bus
}

const maxLog = 1000

// Run shows the dashboard until the user quits or ctx is cancelled.
func Run(ctx context.Context, b Backend, webAddr string) error {
	ch, cancel := b.Bus().Subscribe(1024)
	defer cancel()
	m := &model{b: b, ch: ch, webAddr: webAddr}
	m.refresh()
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type tickMsg time.Time
type eventMsg events.Event
type closedMsg struct{}

type logLine struct {
	t       time.Time
	project string
	level   events.Level
	text    string
}

type model struct {
	b       Backend
	ch      <-chan events.Event
	webAddr string

	snaps  []project.Snapshot
	ports  map[string]string
	cursor int
	focus  int // 0 = projects, 1 = log
	scroll int // log lines scrolled back from the bottom
	log    []logLine
	status string
	width  int
	height int
}

func (m *model) refresh() {
	m.snaps = m.b.Snapshots()
	m.ports = map[string]string{}
	for _, p := range m.b.Projects() {
		var ps []string
		for _, r := range p.Routes {
			ps = append(ps, fmt.Sprintf("%d/%s", r.Listen, r.Mode))
		}
		m.ports[p.Name] = strings.Join(ps, " ")
	}
	if m.cursor >= len(m.snaps) {
		m.cursor = max(0, len(m.snaps)-1)
	}
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// nextEvent waits for the next bus event.
func (m *model) nextEvent() tea.Cmd {
	return func() tea.Msg {
		e, ok := <-m.ch
		if !ok {
			return closedMsg{}
		}
		return eventMsg(e)
	}
}

func (m *model) Init() tea.Cmd { return tea.Batch(tick(), m.nextEvent()) }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		m.refresh()
		return m, tick()
	case closedMsg:
		return m, nil
	case eventMsg:
		e := events.Event(msg)
		if e.Type == events.State {
			m.refresh()
		} else {
			lvl, text := events.Describe(e)
			m.log = append(m.log, logLine{t: e.Time, project: e.Project, level: lvl, text: text})
			if len(m.log) > maxLog {
				m.log = m.log[len(m.log)-maxLog:]
			}
			if m.scroll > 0 {
				m.scroll++
			}
		}
		return m, m.nextEvent()
	case tea.KeyMsg:
		return m, m.key(msg.String())
	}
	return m, nil
}

func (m *model) selected() string {
	if m.cursor < len(m.snaps) {
		return m.snaps[m.cursor].Name
	}
	return ""
}

func (m *model) key(k string) tea.Cmd {
	switch k {
	case "q", "ctrl+c":
		return tea.Quit
	case "tab":
		m.focus = 1 - m.focus
	case "up", "k":
		if m.focus == 0 {
			m.cursor = max(0, m.cursor-1)
		} else {
			m.scroll = min(m.scroll+1, max(0, len(m.log)-1))
		}
	case "down", "j":
		if m.focus == 0 {
			m.cursor = min(m.cursor+1, max(0, len(m.snaps)-1))
		} else {
			m.scroll = max(0, m.scroll-1)
		}
	case "s":
		if n := m.selected(); n != "" {
			m.status = outcome("start "+n, m.b.StartProject(n))
		}
	case "x":
		if n := m.selected(); n != "" {
			m.status = outcome("stop "+n, m.b.StopProject(n))
		}
	}
	return nil
}

func outcome(what string, err error) string {
	if err != nil {
		return what + ": " + err.Error()
	}
	return what + ": requested"
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#a78bfa"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6b7280"))
	headStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#9ca3af"))
	selStyle    = lipgloss.NewStyle().Background(lipgloss.Color("#312e81"))
	panelStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#374151")).Padding(0, 1)
	activePanel = panelStyle.BorderForeground(lipgloss.Color("#8b5cf6"))
	stateColor  = map[project.State]lipgloss.Style{
		project.Running:  lipgloss.NewStyle().Foreground(lipgloss.Color("#34d399")),
		project.Starting: lipgloss.NewStyle().Foreground(lipgloss.Color("#fbbf24")),
		project.Stopping: lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")),
		project.Stopped:  lipgloss.NewStyle().Foreground(lipgloss.Color("#6b7280")),
	}
	levelColor = map[events.Level]lipgloss.Style{
		events.LevelDebug: dimStyle,
		events.LevelInfo:  lipgloss.NewStyle(),
		events.LevelWarn:  lipgloss.NewStyle().Foreground(lipgloss.Color("#fbbf24")),
		events.LevelError: lipgloss.NewStyle().Foreground(lipgloss.Color("#f87171")),
	}
)

// compactDuration renders d as "1h05m", "3m07s" or "12s".
func compactDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func (m *model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	inner := max(20, m.width-4)
	header := titleStyle.Render("dcpentry") + dimStyle.Render("  web http://"+m.webAddr)

	rows := []string{headStyle.Render(pad("PROJECT", 20) + pad("STATE", 11) + pad("PORTS", 24) + pad("CONNS", 7) + pad("UPTIME", 10) + "IDLE IN")}
	for i, s := range m.snaps {
		uptime, idle := "-", "-"
		if s.RunningSince != nil {
			uptime = compactDuration(time.Since(*s.RunningSince))
		}
		if s.IdleDeadline != nil {
			idle = compactDuration(time.Until(*s.IdleDeadline))
		}
		line := pad(s.Name, 20) + pad(stateColor[s.State].Render(string(s.State)), 11) + pad(m.ports[s.Name], 24) +
			pad(fmt.Sprint(s.Conns), 7) + pad(uptime, 10) + idle
		if s.LastError != "" {
			line += levelColor[events.LevelError].Render("  ! " + s.LastError)
		}
		if lipgloss.Width(line) > inner {
			line = lipgloss.NewStyle().MaxWidth(inner).Render(line)
		}
		if i == m.cursor {
			line = selStyle.Render(pad(line, inner))
		}
		rows = append(rows, line)
	}
	if len(m.snaps) == 0 {
		rows = append(rows, dimStyle.Render("no projects configured — add some at http://"+m.webAddr))
	}
	projPanel := panelStyle
	logPanel := panelStyle
	if m.focus == 0 {
		projPanel = activePanel
	} else {
		logPanel = activePanel
	}
	top := projPanel.Width(inner).Render(strings.Join(rows, "\n"))

	footer := dimStyle.Render("↑/↓ select · s start · x stop · tab switch panel · q quit")
	if m.status != "" {
		footer = m.status + "   " + footer
	}
	logH := max(3, m.height-lipgloss.Height(header)-lipgloss.Height(top)-lipgloss.Height(footer)-2)
	end := len(m.log) - m.scroll
	startIdx := max(0, end-logH)
	var lines []string
	for _, l := range m.log[startIdx:end] {
		proj := ""
		if l.project != "" {
			proj = titleStyle.UnsetBold().Render(l.project) + " "
		}
		line := dimStyle.Render(l.t.Format("15:04:05")) + " " + proj + levelColor[l.level].Render(l.text)
		lines = append(lines, lipgloss.NewStyle().MaxWidth(inner).Render(line))
	}
	for len(lines) < logH {
		lines = append(lines, "")
	}
	bottom := logPanel.Width(inner).Render(strings.Join(lines, "\n"))
	return lipgloss.JoinVertical(lipgloss.Left, header, top, bottom, footer)
}
