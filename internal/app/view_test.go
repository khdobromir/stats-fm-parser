package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stats-fm-parser/internal/statsfm"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestPalette(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if got := loadPalette(context.Background()); got["accent"] != "36" {
		t.Fatal(got)
	}
	for _, tc := range []struct{ hex, code string }{{"#7d82d9", "38;2;125;130;217"}, {"#124459", "38;2;18;68;89"}} {
		script := "#!/bin/sh\n[ \"$*\" = 'theme color --all' ] || exit 1\nprintf 'accent\\t" + tc.hex + "\\nselection\\t#eeeeee\\nred\\tinvalid\\n'\n"
		if err := os.WriteFile(filepath.Join(bin, "omarchy"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		p := loadPalette(context.Background())
		if p["accent"] != tc.code || p["selection"] != "48;2;238;238;238" || p["red"] != "31" {
			t.Fatal(p)
		}
	}
	t.Setenv("NO_COLOR", "1")
	if s := loadPalette(context.Background()).paint("accent", "текст"); s != "текст" {
		t.Fatal(s)
	}
}

func TestConsoleLayout(t *testing.T) {
	m := ui{colors: terminalPalette(), o: options{Options: statsfm.Options{User: strings.Repeat("音", 100)}}}
	for _, size := range []tea.WindowSizeMsg{{Width: 120, Height: 36}, {Width: 80, Height: 24}, {Width: 40, Height: 15}, {Width: 15, Height: 7}} {
		m.Update(size)
		for _, stage := range []string{"form", "running", "done"} {
			m.running = stage == "running"
			m.latest = update{done: stage == "done", Update: statsfm.Update{State: "interrupted", Dir: strings.Repeat("длинный каталог/", 25), Progress: statsfm.Progress{Path: "top/tracks", Period: "weeks", Order: "COUNT", Records: 78, Total: 17}}}
			v := m.View()
			lines := strings.Split(v.Content, "\n")
			if !v.AltScreen || len(lines) != size.Height {
				t.Fatal(size, stage)
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size.Width {
					t.Fatalf("%s %v: %q", stage, size, line)
				}
			}
			if size.Width >= 40 && stage == "form" && !strings.Contains(v.Content, "› Пользователь") {
				t.Fatal("missing focus")
			}
			if size.Width >= 40 && stage == "running" && !strings.Contains(v.Content, "78") {
				t.Fatal("missing record count")
			}
		}
	}
}
func TestResultRemainsUntilExit(t *testing.T) {
	m := ui{running: true, width: 40, height: 15}
	for _, tc := range []struct {
		state, label string
		err          error
	}{
		{"finished", "Экспорт завершён", nil}, {"finished_with_gaps", "с пропусками", nil},
		{"interrupted", "-resume", context.Canceled}, {"save_error", "disk full", errors.Join(context.Canceled, errors.New("disk full"))}, {"", "bad profile", errors.New("bad profile")},
	} {
		_, cmd := m.Update(update{done: true, err: tc.err, Update: statsfm.Update{State: tc.state, Dir: "/tmp/test", Progress: statsfm.Progress{Records: 321}}})
		lines := strings.Join(m.resultLines(80), "\n")
		if cmd != nil || m.running || !strings.Contains(lines, tc.label) {
			t.Fatal(lines)
		}
		if tc.state == "save_error" && strings.Contains(lines, "Сохранено записей") {
			t.Fatal(lines)
		}
		if _, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd == nil {
			t.Fatal("Enter must exit")
		}
	}
}

func TestChoosePeriod(t *testing.T) {
	m := ui{width: 40, height: 15}
	m.Update(tea.PasteMsg{Content: "volt"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for _, label := range []string{"За неделю", "За 4 недели", "За 6 месяцев", "За текущий год", "Свои даты"} {
		m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, label) {
			t.Fatalf("missing selected option %q in %q", label, view)
		}
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(tea.PasteMsg{Content: "2026-01-01"})
	if m.o.From != "2026-01-01" {
		t.Fatal("Enter on custom period should focus the start date")
	}
}

func TestDatesAndInput(t *testing.T) {
	m := ui{}
	m.Update(tea.PasteMsg{Content: "volt\n"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.o.User != "volt" || m.field != 1 {
		t.Fatal(m.o, m.field)
	}
}
func TestSpotifyForm(t *testing.T) {
	m := ui{width: 40, height: 15}
	m.Update(tea.PasteMsg{Content: "volt"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if view := m.View().Content; !strings.Contains(view, "Любимые треки") || !strings.Contains(view, "Путь") {
		t.Fatal(view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if view := m.View().Content; m.running || !strings.Contains(view, "Укажите путь") {
		t.Fatal(view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.o.User != "volt" || !strings.Contains(m.View().Content, "Пользователь") {
		t.Fatal("draft lost when switching sources")
	}
}

func TestHistoryModeAndUpdateForm(t *testing.T) {
	m := ui{width: 85, height: 24}
	if !strings.Contains(m.View().Content, "История и метаданные") {
		t.Fatal("history mode should be the default")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF4})
	if m.o.Mode != "full" || !strings.Contains(m.View().Content, "Полный экспорт") {
		t.Fatal("F4 should select full mode")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF3})
	if !strings.Contains(m.View().Content, "Обновить архив") {
		t.Fatal("F3 should open the update form")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.running || !strings.Contains(m.View().Content, "Укажите путь") {
		t.Fatal("the archive path is required")
	}
	m.Update(tea.PasteMsg{Content: "/tmp/history"})
	if m.archivePath != "/tmp/history" {
		t.Fatal(m.archivePath)
	}
}
