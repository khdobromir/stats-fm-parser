package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

type options struct{ user, from, to, out, resume string }

func (o options) validate() error {
	if o.resume != "" {
		if o.user != "" || o.from != "" || o.to != "" {
			return errors.New("-resume uses the original username and dates")
		}
		return nil
	}
	if !safeSegment(o.user) {
		return errors.New("введите username из профиля stats.fm")
	}
	if o.from != "" || o.to != "" {
		_, _, err := dateBounds(o.from, o.to)
		return err
	}
	return nil
}

type update struct {
	dir  string
	p    progress
	done bool
	err  error
}

func export(ctx context.Context, c *client, o options, report func(update)) (string, error) {
	if err := o.validate(); err != nil {
		return "", err
	}
	dir := o.resume
	var cfg *config
	if dir == "" {
		p, err := c.profile(ctx, o.user)
		if err != nil {
			return "", err
		}
		periods := []string{"today", "days", "weeks", "months", "lifetime"}
		if o.from != "" {
			periods = append(periods, "custom")
		}
		cfg = &config{p, periods, o.from, o.to, time.Now().UTC()}
		if err = os.MkdirAll(o.out, 0700); err != nil {
			return "", err
		}
		dir, err = os.MkdirTemp(o.out, "statsfm-"+cfg.Started.Format("20060102-150405")+"-")
		if err != nil {
			return "", err
		}
	}
	s, err := openStore(dir, cfg)
	if err != nil {
		return dir, err
	}
	defer s.close()
	report(update{dir: dir})
	idx := newIndex()
	if err = s.replay(func(p page) error { idx.apply(p); return nil }); err != nil {
		return dir, err
	}
	if err = writeJSON(dir, "manifest.json", object{"version": 1, "session": s.cfg, "state": "running"}); err != nil {
		return dir, err
	}
	err = collect(ctx, c, s, idx, func(p progress) { report(update{dir: dir, p: p}) })
	state := "finished"
	if err != nil {
		state = "interrupted"
	}
	report(update{dir: dir, p: progress{Path: "Запись JSON и JSONL", Records: idx.count}})
	return dir, errors.Join(err, s.publish(idx, state, err))
}

type ui struct {
	o       options
	field   int
	running bool
	note    string
	latest  update
	events  chan update
	cancel  context.CancelFunc
	c       *client
	ctx     context.Context
}

func (m *ui) Init() tea.Cmd   { return nil }
func (m *ui) listen() tea.Msg { return <-m.events }
func (m *ui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch x := msg.(type) {
	case update:
		if x.done {
			m.latest.dir = x.dir
			m.latest.err = x.err
			return m, tea.Quit
		}
		m.latest = x
		return m, m.listen
	case tea.KeyPressMsg:
		switch x.String() {
		case "esc", "ctrl+c":
			if m.running {
				m.cancel()
				m.note = "Останавливаю экспорт и сохраняю результат…"
				return m, nil
			}
			return m, tea.Quit
		}
		if m.running {
			return m, nil
		}
		fields := []*string{&m.o.user, &m.o.from, &m.o.to}
		switch x.String() {
		case "up", "shift+tab":
			m.field = (m.field + 2) % 3
		case "down", "tab":
			m.field = (m.field + 1) % 3
		case "enter":
			if m.field < 2 {
				m.field++
				return m, nil
			}
			if err := m.o.validate(); err != nil {
				m.note = err.Error()
				return m, nil
			}
			ctx, cancel := context.WithCancel(m.ctx)
			m.cancel = cancel
			m.running = true
			m.note = ""
			c, o, ch := m.c, m.o, m.events
			return m, tea.Batch(m.listen, func() tea.Msg {
				dir, err := export(ctx, c, o, func(u update) {
					select {
					case ch <- u:
					default:
					}
				})
				ch <- update{dir: dir, done: true, err: err}
				return nil
			})
		case "backspace":
			r := []rune(*fields[m.field])
			if len(r) > 0 {
				*fields[m.field] = string(r[:len(r)-1])
			}
		default:
			*fields[m.field] += cleanText(x.Text)
		}
	case tea.PasteMsg:
		if !m.running {
			fields := []*string{&m.o.user, &m.o.from, &m.o.to}
			*fields[m.field] += cleanText(x.Content)
		}
	}
	return m, nil
}
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
func (m *ui) View() tea.View {
	var b strings.Builder
	b.WriteString("stats.fm  ·  Экспорт музыкальной статистики\n\n")
	if !m.running {
		for i, x := range []string{"Username  " + m.o.user, "С даты    " + m.o.from, "По дату   " + m.o.to} {
			mark := "  "
			if i == m.field {
				mark = "› "
			}
			fmt.Fprintln(&b, mark+x)
		}
		b.WriteString("\nДаты необязательны · YYYY-MM-DD (UTC)\nEnter — дальше или начать · Tab — выбрать поле · Esc — выход\n")
	} else {
		fmt.Fprintf(&b, "%s\nЗаписей: %d · Объектов: %d/%d\n%s\nEsc — остановить и сохранить\n", m.latest.p.Path, m.latest.p.Records, m.latest.p.Done, m.latest.p.Total, m.latest.dir)
	}
	b.WriteString(m.note)
	return tea.NewView(b.String())
}
func run() error {
	var o options
	flag.StringVar(&o.user, "user", "", "stats.fm username (batch mode)")
	flag.StringVar(&o.from, "from", "", "first UTC date, YYYY-MM-DD")
	flag.StringVar(&o.to, "to", "", "last UTC date, YYYY-MM-DD")
	flag.StringVar(&o.out, "out", "exports", "parent directory for new exports")
	flag.StringVar(&o.resume, "resume", "", "resume a saved export directory")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("use -user USERNAME or run without arguments for the TUI")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &client{base: apiBase, http: &http.Client{Timeout: 45 * time.Second}, pace: 200 * time.Millisecond}
	var dir string
	var err error
	if o.user != "" || o.resume != "" {
		last := time.Time{}
		dir, err = export(ctx, c, o, func(u update) {
			if u.p.Path == "" || time.Since(last) > 2*time.Second {
				fmt.Fprintf(os.Stderr, "%s  %s  %d records\n", u.dir, u.p.Path, u.p.Records)
				last = time.Now()
			}
		})
	} else {
		m := &ui{o: o, c: c, ctx: ctx, events: make(chan update, 1)}
		_, err = tea.NewProgram(m).Run()
		if m.cancel != nil {
			m.cancel()
		}
		if err == nil {
			dir, err = m.latest.dir, m.latest.err
		}
	}
	if dir != "" {
		fmt.Fprintln(os.Stderr, "Экспорт:", filepath.Clean(dir))
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "Для продолжения передайте этот каталог через -resume.")
		}
	}
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
