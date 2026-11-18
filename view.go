package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func operation(q query) string {
	parts := strings.Split(q.Path, "/")
	title := "Поиск профиля"
	switch parts[0] {
	case "publish":
		return "Запись JSON и JSONL"
	case "top":
		title = "Топ " + map[string]string{"tracks": "треков", "artists": "исполнителей", "albums": "альбомов", "genres": "жанров"}[parts[len(parts)-1]]
		if len(parts) == 4 {
			title += " · ID " + parts[2]
		}
	case "streams":
		title = "История прослушиваний"
		switch parts[len(parts)-1] {
		case "recent":
			title = "Недавние прослушивания"
		case "current":
			title = "Сейчас играет"
		case "stats":
			title = "Сводная статистика"
		case "dates":
			title = "Статистика по датам"
		case "per-day":
			title = "Статистика по дням"
		}
	case "records":
		title = "Рекорды исполнителей"
	case "recaps":
		title = "Ретроспективы"
	}
	if p := map[string]string{"week": "7 дней", "ytd": "с начала года", "today": "сегодня", "days": "последние дни", "weeks": "4 недели", "months": "6 месяцев", "lifetime": "весь период", "custom": "свои даты"}[q.Period]; p != "" {
		title += " · " + p
	}
	if o := map[string]string{"COUNT": "по прослушиваниям", "TIME": "по времени", "SPOTIFY": "Spotify", "APPLEMUSIC": "Apple Music", "default": "по умолчанию"}[q.Order]; o != "" {
		title += " · " + o
	}
	return title
}

func fit(s string, w int) string { return ansi.Truncate(s, max(1, w), "…") }
func pad(s string, w int) string {
	s = fit(s, w)
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}
func (m *ui) heading(s string, w int) string {
	return m.colors.paint("accent", s) + " " + m.colors.paint("muted", strings.Repeat("─", max(0, w-ansi.StringWidth(s)-1)))
}

func (m *ui) form(w, h int) []string {
	wide := w >= 76
	fw := w
	if wide {
		fw = (w - 4) * 3 / 5
	}
	lines := []string{m.heading("Профиль", fw), ""}
	labels := []string{"Пользователь", "Период", "С даты", "По дату"}
	values := []string{m.o.user, periodChoices[m.selected].label, m.o.from, m.o.to}[:m.lastField()+1]
	for i, val := range values {
		if h < 8 && i != m.field {
			continue
		}
		active := i == m.field
		prefix := "  "
		if active {
			prefix = "› "
		}
		label := prefix + labels[i] + "  "
		if val == "" {
			val = []string{"username", "", "YYYY-MM-DD", "YYYY-MM-DD"}[i]
		}
		val = cleanText(val)
		if active {
			width := max(1, fw-ansi.StringWidth(label)-2)
			if n := ansi.StringWidth(val); n > width {
				val = ansi.TruncateLeft(val, n-width, "")
			}
			input := val + "▏"
			if i == 1 {
				input = "‹ " + val + " ›"
			}
			line := pad(label+input, fw)
			lines = append(lines, m.colors.paint("selection", m.colors.paint("accent", line)))
			if m.note != "" {
				lines = append(lines, strings.Split(ansi.Wrap(m.colors.paint("red", "! "+m.note), fw, ""), "\n")...)
			}
		} else {
			lines = append(lines, fit(m.colors.paint("muted", label)+m.colors.paint("foreground", val), fw))
		}
	}
	details := []string{m.heading("Выгрузка", w-fw-4), "", "Треки · Исполнители · Альбомы · Жанры", "История и статистика"}
	if m.o.period == "" {
		details[3] += " · Ретроспективы"
	}
	if m.o.period == customPeriod {
		details = append(details, "Даты включительно (UTC)")
	}
	if !wide {
		if h >= len(lines)+len(details)+1 {
			details[0] = m.heading("Выгрузка", w)
			lines = append(lines, "")
			lines = append(lines, details...)
		} else if m.o.period == customPeriod && h > len(lines) {
			lines = append(lines, "Даты включительно (UTC)")
		}
		return lines
	}
	for len(lines) < len(details) {
		lines = append(lines, "")
	}
	for i, s := range details {
		lines[i] = pad(lines[i], fw) + "    " + fit(s, w-fw-4)
	}
	return lines
}

func (m *ui) resultLines(w int) []string {
	u := m.latest
	label, role := "Ошибка экспорта", "red"
	switch u.state {
	case "finished":
		label, role = "Экспорт завершён", "green"
	case "finished_with_gaps":
		label, role = "Экспорт завершён с пропусками", "yellow"
	case "interrupted":
		label, role = "Экспорт прерван", "yellow"
	case "save_error":
		label = "Ошибка сохранения"
	default:
		if errors.Is(u.err, context.Canceled) {
			label, role = "Экспорт отменён", "yellow"
		}
	}
	lines := []string{m.colors.paint(role, label)}
	add := func(s string) { lines = append(lines, strings.Split(ansi.Hardwrap(s, w, true), "\n")...) }
	switch u.state {
	case "finished", "finished_with_gaps", "interrupted":
		add(fmt.Sprintf("Сохранено записей   %d", u.p.Records))
	}
	if u.dir != "" {
		add("Каталог   " + cleanText(u.dir))
	}
	if u.state == "finished_with_gaps" {
		add("Причины пропусков — в manifest.json")
	}
	if u.err != nil && (u.state == "save_error" || !errors.Is(u.err, context.Canceled)) {
		add(cleanText(strings.ReplaceAll(u.err.Error(), "\n", " · ")))
	}
	if u.state == "interrupted" || u.state == "save_error" {
		path := strings.ReplaceAll(cleanText(u.dir), "'", "'\"'\"'")
		add("Продолжить из каталога проекта")
		add("go run . -resume '" + path + "'")
	}
	return lines
}

func (m *ui) contentWidth() int {
	margin := 1
	switch {
	case m.width >= 76:
		margin = 3
	case m.width < 8:
		margin = 0
	}
	return max(1, min(m.width-2*margin, 112))
}
func (m *ui) View() tea.View {
	w, h := max(1, m.width), max(1, m.height)
	contentW := m.contentWidth()
	left := strings.Repeat(" ", (w-contentW)/2)
	title, keys := "Новый экспорт", "Tab выбор  ·  Enter далее  ·  Esc выход"
	if contentW < 42 {
		keys = "Tab поле · Enter далее · Esc выход"
	}
	if m.field == m.lastField() {
		keys = strings.Replace(keys, "далее", "старт", 1)
	}
	if m.field == 1 {
		keys = "←→ период · Enter далее · Esc выход"
		if m.lastField() == 1 {
			keys = strings.Replace(keys, "далее", "старт", 1)
		}
	}
	var body []string
	switch {
	case m.latest.done:
		title, keys = "Результат", "Enter / Esc выход"
		body = m.resultLines(contentW)
	case m.running:
		title, keys = "Экспорт", "Esc остановить и сохранить"
		p := m.latest.p
		body = []string{m.heading("Текущая операция", contentW)}
		body = append(body, strings.Split(ansi.Wrap(m.colors.paint("accent", operation(p.query)), contentW, ""), "\n")...)
		body = append(body, "", fmt.Sprintf("Записей     %s", m.colors.paint("accent", fmt.Sprint(p.Records))), fmt.Sprintf("Объектов    %d из %d", p.Done, p.Total))
		if m.note != "" {
			body = append(body, m.colors.paint("yellow", m.note))
		}
	default:
		body = m.form(contentW, h-4)
	}
	header := m.colors.paint("accent", "▂▄▆  stats.fm") + "   /   " + title
	room := max(0, h-3)
	if m.latest.done && len(body) > room {
		keys = "↑↓ листать  ·  Enter / Esc выход"
		start := min(m.scroll, max(0, len(body)-room))
		body = body[start:]
	}
	rows := []string{header, m.colors.paint("muted", strings.Repeat("─", contentW))}
	slack := max(0, room-len(body))
	if h >= 18 {
		rows = append(rows, make([]string, slack/3)...)
	}
	rows = append(rows, body...)
	if len(rows) > h-1 {
		rows = rows[:h-1]
	}
	for len(rows) < h-1 {
		rows = append(rows, "")
	}
	rows = append(rows, m.colors.paint("muted", keys))
	for i, s := range rows {
		if s != "" {
			rows[i] = left + fit(s, contentW)
		}
	}
	v := tea.NewView(strings.Join(rows, "\n"))
	v.AltScreen = true
	return v
}
