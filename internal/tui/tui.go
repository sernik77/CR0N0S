// Package tui is the interactive "cr0 ui" interface.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sernik77/CR0N0S/internal/cond"
	"github.com/sernik77/CR0N0S/internal/core"
	"github.com/sernik77/CR0N0S/internal/model"
	"github.com/sernik77/CR0N0S/internal/runner"
	"github.com/sernik77/CR0N0S/internal/store"
)

// ToggleFunc applies arm/disarm/pause/resume to a job.
type ToggleFunc func(j *model.Job, action string, now time.Time) error

type view int

const (
	viewList view = iota
	viewInspect
	viewHistory
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	headStyle  = lipgloss.NewStyle().Bold(true).Underline(true)
	selStyle   = lipgloss.NewStyle().Reverse(true)
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	badStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
)

type tickMsg time.Time
type statusMsg string

type model_ struct {
	st      *store.Store
	toggle  ToggleFunc
	jobs    []*model.Job
	ins     map[string]core.Inspection
	cursor  int
	view    view
	all     bool
	filter  string
	editing bool
	status  string
	width   int
	height  int
	err     error
}

// Run starts the TUI.
func Run(st *store.Store, toggle ToggleFunc) error {
	m := &model_{st: st, toggle: toggle}
	m.reload()
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model_) Init() tea.Cmd { return tick() }

func (m *model_) reload() {
	jobs, err := m.st.List()
	if err != nil {
		m.err = err
		return
	}
	now := time.Now()
	m.jobs, m.ins = m.jobs[:0], map[string]core.Inspection{}
	for _, j := range jobs {
		if !m.all && j.Status != model.StatusActive {
			continue
		}
		if m.filter != "" && !strings.Contains(strings.ToLower(j.Title+" "+j.Command+j.Script+" "+j.Path), strings.ToLower(m.filter)) {
			continue
		}
		m.jobs = append(m.jobs, j)
		m.ins[j.UUID] = core.Inspect(m.st, j, now)
	}
	if m.cursor >= len(m.jobs) {
		m.cursor = max(0, len(m.jobs)-1)
	}
}

func (m *model_) selected() *model.Job {
	if m.cursor < len(m.jobs) {
		return m.jobs[m.cursor]
	}
	return nil
}

func (m *model_) apply(action string) {
	j := m.selected()
	if j == nil {
		return
	}
	_, err := m.st.Update(j.UUID, store.Audit{Action: action, Reason: "via cr0 ui"}, func(cur *model.Job) error {
		return m.toggle(cur, action, time.Now())
	})
	if err != nil {
		m.status = badStyle.Render(err.Error())
	} else {
		m.status = fmt.Sprintf("%s %s", action, j.Title)
	}
	m.reload()
}

func (m *model_) runNow() tea.Cmd {
	j := m.selected()
	if j == nil {
		return nil
	}
	m.status = "running " + j.Title + "…"
	st := m.st
	return func() tea.Msg {
		run, err := core.Execute(context.Background(), st, runner.New(), j, core.TriggerManual)
		if err != nil {
			return statusMsg(badStyle.Render(err.Error()))
		}
		return statusMsg(fmt.Sprintf("run #%d %s: %s exit=%d %s", run.ID, j.Title, run.Status, run.ExitCode, run.Error))
	}
}

func (m *model_) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		m.reload()
		return m, tick()
	case statusMsg:
		m.status = string(msg)
		m.reload()
	case tea.KeyMsg:
		if m.editing {
			switch msg.Type {
			case tea.KeyEnter, tea.KeyEsc:
				m.editing = false
			case tea.KeyBackspace:
				if m.filter != "" {
					m.filter = m.filter[:len(m.filter)-1]
				}
			case tea.KeyRunes, tea.KeySpace:
				m.filter += string(msg.Runes)
			}
			m.reload()
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.view = viewList
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.jobs)-1 {
				m.cursor++
			}
		case "enter", "i":
			if m.selected() != nil {
				m.view = viewInspect
			}
		case "h":
			m.view = viewHistory
		case "l":
			m.view = viewList
		case "a":
			m.all = !m.all
			m.reload()
		case "/":
			m.editing, m.filter = true, ""
		case "m":
			if j := m.selected(); j != nil {
				m.apply(map[bool]string{true: "disarm", false: "arm"}[j.Armed])
			}
		case "p":
			if j := m.selected(); j != nil {
				m.apply(map[bool]string{true: "resume", false: "pause"}[j.Paused])
			}
		case "r":
			return m, m.runNow()
		case "g":
			m.reload()
		}
	}
	return m, nil
}

func (m *model_) View() string {
	if m.err != nil {
		return badStyle.Render("error: "+m.err.Error()) + "\n"
	}
	var b strings.Builder
	scope := "active"
	if m.all {
		scope = "all"
	}
	b.WriteString(titleStyle.Render("CR0N0S") + dimStyle.Render(fmt.Sprintf("  %d %s jobs  db %s", len(m.jobs), scope, m.st.Path)) + "\n\n")
	switch m.view {
	case viewInspect:
		m.viewInspect(&b)
	case viewHistory:
		m.viewHistory(&b)
	default:
		m.viewList(&b)
	}
	b.WriteString("\n")
	if m.editing {
		b.WriteString("filter: " + m.filter + "█\n")
	} else if m.filter != "" {
		b.WriteString(dimStyle.Render("filter: "+m.filter) + "\n")
	}
	if m.status != "" {
		b.WriteString(m.status + "\n")
	}
	b.WriteString(dimStyle.Render("↑/↓ move · enter inspect · h history · l list · m arm/disarm · p pause/resume · r run now · a all/active · / filter · q quit"))
	return b.String()
}

func (m *model_) viewList(b *strings.Builder) {
	now := time.Now()
	b.WriteString(headStyle.Render(fmt.Sprintf("%-8s  %-26s  %-20s  %-12s  %-14s  %-8s  %s", "ID", "TITLE", "SCHEDULE", "IN", "STATE", "LAST", "COND")) + "\n")
	if len(m.jobs) == 0 {
		b.WriteString(dimStyle.Render("no jobs — register one with 'cr0 job register'") + "\n")
	}
	rows := m.height - 8
	start := 0
	if rows > 0 && m.cursor >= rows {
		start = m.cursor - rows + 1
	}
	for i := start; i < len(m.jobs) && (rows <= 0 || i < start+rows); i++ {
		j := m.jobs[i]
		in := "-"
		if j.NextRun != nil && !j.Paused {
			in = j.NextRun.Sub(now).Round(time.Second).String()
		}
		ins := m.ins[j.UUID]
		c := "-"
		if ins.Count > 0 {
			c = fmt.Sprintf("%d/%d %v", ins.MetCount, ins.Count, ins.Met)
		}
		line := fmt.Sprintf("%-8s  %-26s  %-20s  %-12s  %-14s  %-8s  %s", j.UUID[:8], trunc(j.Title, 26), trunc(schedText(j.Schedule), 20),
			trunc(in, 12), stateText(j), trunc(j.LastStatus, 8), c)
		if i == m.cursor {
			line = selStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
}

func (m *model_) viewInspect(b *strings.Builder) {
	j := m.selected()
	if j == nil {
		return
	}
	now := time.Now()
	kv := func(k, v string) {
		if v != "" {
			fmt.Fprintf(b, "%s %s\n", dimStyle.Render(fmt.Sprintf("%-12s", k)), v)
		}
	}
	b.WriteString(titleStyle.Render(j.Title) + dimStyle.Render("  "+j.UUID) + "\n")
	kv("description", j.Description)
	kv("state", j.Status+" / "+stateText(j))
	kv("schedule", schedText(j.Schedule))
	if j.NextRun != nil {
		kv("next run", j.NextRun.Local().Format("2006-01-02 15:04:05")+" (in "+j.NextRun.Sub(now).Round(time.Second).String()+")")
	}
	kv("registered", j.RegisteredAt.Local().Format("2006-01-02 15:04:05"))
	kv("path", j.Path)
	kv("user", j.User)
	if j.Superuser {
		kv("superuser", j.SudoMode)
	}
	kv("command", j.Command)
	kv("script", j.Script)
	kv("interpreter", j.Interpreter)
	ins := m.ins[j.UUID]
	met := badStyle.Render("false")
	if ins.Met {
		met = okStyle.Render("true")
	}
	fmt.Fprintf(b, "\nconditions: %d, met %d, conditions met: %s\n", ins.Count, ins.MetCount, met)
	if ins.Tree != nil {
		tree(b, *ins.Tree, "  ")
	}
	runs, _ := m.st.Runs(j.UUID, 8)
	if len(runs) > 0 {
		b.WriteString("\nlast runs:\n")
		for _, r := range runs {
			fmt.Fprintf(b, "  #%-4d %s  %-8s %s exit=%d %s\n", r.ID, r.StartedAt.Format("01-02 15:04:05"), r.Trigger, colorStatus(r.Status), r.ExitCode, r.Error)
		}
	}
}

func (m *model_) viewHistory(b *strings.Builder) {
	runs, err := m.st.Runs("", max(10, m.height-8))
	if err != nil {
		b.WriteString(badStyle.Render(err.Error()))
		return
	}
	b.WriteString(headStyle.Render(fmt.Sprintf("%-6s  %-14s  %-8s  %-9s  %-8s  %-5s  %s", "RUN", "STARTED", "JOB", "TRIGGER", "STATUS", "EXIT", "NOTE")) + "\n")
	for _, r := range runs {
		fmt.Fprintf(b, "%-6d  %-14s  %-8s  %-9s  %s  %-5d  %s\n", r.ID, r.StartedAt.Format("01-02 15:04:05"), r.JobUUID[:8], r.Trigger,
			colorStatus(fmt.Sprintf("%-8s", r.Status)), r.ExitCode, trunc(r.Error, 50))
	}
}

func tree(b *strings.Builder, r cond.Result, indent string) {
	mark := badStyle.Render("✗")
	if r.Met {
		mark = okStyle.Render("✓")
	}
	fmt.Fprintf(b, "%s%s %s", indent, mark, r.Desc)
	if r.Err != "" {
		b.WriteString(dimStyle.Render("  (" + r.Err + ")"))
	}
	b.WriteString("\n")
	for _, c := range r.Children {
		tree(b, c, indent+"  ")
	}
}

func colorStatus(s string) string {
	switch strings.TrimSpace(s) {
	case model.RunSuccess:
		return okStyle.Render(s)
	case model.RunFail, model.RunError:
		return badStyle.Render(s)
	}
	return warnStyle.Render(s)
}

func schedText(s model.Schedule) string {
	switch s.Kind {
	case model.ScheduleCron:
		return "cron " + s.Cron
	case model.ScheduleInterval:
		return "every " + s.Every.String()
	case model.ScheduleWatch:
		return "watch " + strings.Join(s.Watch, ",")
	}
	return s.Kind
}

func stateText(j *model.Job) string {
	if j.Status != model.StatusActive {
		return j.Status
	}
	s := "armed"
	if !j.Armed {
		s = "disarmed"
	}
	if j.Paused {
		s += ",paused"
	}
	return s
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
