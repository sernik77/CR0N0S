package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sernik77/CR0N0S/internal/model"
	"github.com/sernik77/CR0N0S/internal/sched"
	"github.com/sernik77/CR0N0S/internal/store"
)

func toggle(j *model.Job, action string, now time.Time) error {
	switch action {
	case "arm":
		j.Armed = true
	case "disarm":
		j.Armed = false
	case "pause":
		sched.Pause(j, now)
	case "resume":
		return sched.Resume(j, now)
	}
	return nil
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestHeadless(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "cr0.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().Add(time.Hour)
	for _, title := range []string{"alpha", "beta"} {
		j := &model.Job{Title: title, Path: "/tmp", Command: "true", Armed: true, NextRun: &at,
			Schedule:   model.Schedule{Kind: model.ScheduleOnce, At: at},
			Conditions: &model.Condition{Type: "env", Name: "CR0_TUI_UNSET"}}
		if err := st.Register(j); err != nil {
			t.Fatal(err)
		}
	}
	m := &model_{st: st, toggle: toggle}
	m.reload(true)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if v := m.View(); !strings.Contains(v, "alpha") || !strings.Contains(v, "0/1 false") {
		t.Fatalf("list view:\n%s", v)
	}
	for _, k := range []string{"j", "m", "p", "enter"} {
		m.Update(key(k))
	}
	if v := m.View(); !strings.Contains(v, "conditions: 1, met 0") {
		t.Fatalf("inspect view:\n%s", v)
	}
	sel, _ := st.Get(m.selected().UUID)
	if sel.Armed || !sel.Paused {
		t.Fatalf("toggles not applied: armed=%v paused=%v", sel.Armed, sel.Paused)
	}
	for _, k := range []string{"esc", "h", "l", "/", "a", "l", "p", "enter"} {
		m.Update(key(k))
	}
	if m.filter != "alp" || len(m.jobs) != 1 {
		t.Fatalf("filter %q jobs %d", m.filter, len(m.jobs))
	}
	m.status = "" // the status line still mentions the last toggled job
	if v := m.View(); !strings.Contains(v, "alpha") || strings.Contains(v, "beta") {
		t.Fatalf("filtered view:\n%s", v)
	}
}
