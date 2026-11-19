package app

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"stats-fm-parser/internal/statsfm"

	tea "charm.land/bubbletea/v2"
)

type ui struct {
	width, height, scroll int
	colors                palette
	o                     options
	field, selected       int
	running               bool
	spotify               bool
	libraryPath           string
	updating              bool
	archivePath           string
	note                  string
	latest                update
	events                chan update
	cancel                context.CancelFunc
	ctx                   context.Context
}

func (m *ui) lastField() int {
	if m.o.Period == statsfm.CustomPeriod {
		return 3
	}
	return 1
}

func (m *ui) Init() tea.Cmd   { return nil }
func (m *ui) listen() tea.Msg { return <-m.events }
func (m *ui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch x := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = x.Width, x.Height
		return m, nil
	case update:
		m.latest = x
		if x.done {
			m.running = false
			m.note = ""
			return m, nil
		}
		return m, m.listen
	case tea.KeyPressMsg:
		if m.latest.done {
			switch x.String() {
			case "enter", "esc", "ctrl+c":
				return m, tea.Quit
			case "up":
				m.scroll = max(0, m.scroll-1)
			case "down":
				m.scroll = min(m.scroll+1, max(0, len(m.resultLines(m.contentWidth()))-max(0, m.height-3)))
			}
			return m, nil
		}
		switch x.String() {
		case "esc", "ctrl+c":
			if m.running {
				m.cancel()
				m.note = "Останавливаю экспорт и сохраняю результат…"
				if m.spotify {
					m.note = "Отменяю выгрузку…"
				}
				return m, nil
			}
			return m, tea.Quit
		}
		if m.running {
			return m, nil
		}
		if x.String() == "f2" {
			m.spotify = !m.spotify
			m.updating = false
			m.note = ""
			return m, nil
		}
		if x.String() == "f3" {
			m.updating = !m.updating
			m.spotify = false
			m.note = ""
			return m, nil
		}
		if m.updating {
			switch x.String() {
			case "enter":
				if strings.TrimSpace(m.archivePath) == "" {
					m.note = "Укажите путь к архиву истории"
					return m, nil
				}
				return m, m.start(options{Options: statsfm.Options{Update: m.archivePath}})
			case "backspace":
				m.archivePath = withoutLastRune(m.archivePath)
				m.note = ""
			default:
				m.archivePath += cleanText(x.Text)
				if x.Text != "" {
					m.note = ""
				}
			}
			return m, nil
		}

		if m.spotify {
			switch x.String() {
			case "enter":
				if strings.TrimSpace(m.libraryPath) == "" {
					m.note = "Укажите путь к ZIP или YourLibrary.json"
					return m, nil
				}
				return m, m.start(options{spotifyLibrary: m.libraryPath, Options: statsfm.Options{Out: m.o.Out}})
			case "backspace":
				m.libraryPath = withoutLastRune(m.libraryPath)
				m.note = ""
			default:
				m.libraryPath += cleanText(x.Text)
				if x.Text != "" {
					m.note = ""
				}
			}
			return m, nil
		}
		if x.String() == "f4" {
			if m.o.Mode == "full" {
				m.o.Mode = statsfm.HistoryMode
			} else {
				m.o.Mode = "full"
			}
			return m, nil
		}

		fields := []*string{&m.o.User, nil, &m.o.From, &m.o.To}
		switch x.String() {
		case "up", "shift+tab":
			m.note = ""
			m.field = (m.field + m.lastField()) % (m.lastField() + 1)
		case "down", "tab":
			m.note = ""
			m.field = (m.field + 1) % (m.lastField() + 1)
		case "left", "right":
			if m.field == 1 {
				delta := 1
				if x.String() == "left" {
					delta = -1
				}
				m.selected = (m.selected + delta + len(periodChoices)) % len(periodChoices)
				m.o.Period = periodChoices[m.selected].key
				m.note = ""
			}
		case "enter":
			if m.field < m.lastField() {
				m.note = ""
				m.field++
				return m, nil
			}
			o := m.o
			if o.Mode == "" {
				o.Mode = statsfm.HistoryMode
			}
			if o.Period != statsfm.CustomPeriod {
				o.From, o.To = "", ""
			}
			if err := o.validate(); err != nil {
				m.note = err.Error()
				switch {
				case statsfm.ValidateUsername(m.o.User) != nil:
					m.field = 0
				case !validDate(m.o.From):
					m.field = 2
				default:
					m.field = 3
				}
				return m, nil
			}
			return m, m.start(o)
		case "backspace":
			if m.field == 1 {
				return m, nil
			}
			m.note = ""
			*fields[m.field] = withoutLastRune(*fields[m.field])
		default:
			if m.field != 1 && x.Text != "" {
				m.note = ""
				*fields[m.field] += cleanText(x.Text)
			}
		}
	case tea.PasteMsg:
		if !m.running && !m.latest.done && m.updating {
			m.archivePath += cleanText(x.Content)
			m.note = ""
		} else if !m.running && !m.latest.done && m.spotify {
			m.libraryPath += cleanText(x.Content)
			m.note = ""
		} else if !m.running && !m.latest.done && m.field != 1 {
			m.note = ""
			fields := []*string{&m.o.User, nil, &m.o.From, &m.o.To}
			*fields[m.field] += cleanText(x.Content)
		}
	}
	return m, nil
}
func (m *ui) start(o options) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.running = true
	m.note = ""
	ch := m.events
	return tea.Batch(m.listen, func() tea.Msg {
		var final update
		dir, err := export(ctx, o, func(u update) {
			final = u
			select {
			case ch <- u:
			default:
			}
		})
		final.Dir, final.done, final.err = dir, true, err
		ch <- final
		return nil
	})
}
func withoutLastRune(s string) string {
	_, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size]
}
func validDate(s string) bool { _, err := time.Parse(time.DateOnly, s); return err == nil }
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
