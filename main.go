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
	state string
	dir   string
	p     progress
	done  bool
	err   error
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
	var last progress
	err = collect(ctx, c, s, idx, func(p progress) { last = p; report(update{dir: dir, p: p}) })
	state := "finished"
	for _, r := range idx.results {
		if r.Status != "complete" {
			state = "finished_with_gaps"
			break
		}
	}
	if err != nil {
		state = "interrupted"
	}
	last.query = query{Path: "publish"}
	last.Records = idx.count
	last.Total = len(idx.entities)
	if err == nil {
		last.Done = last.Total
	}
	report(update{dir: dir, p: last})
	saveErr := s.publish(idx, state, err)
	if saveErr != nil {
		state = "save_error"
	}
	report(update{dir: dir, p: last, state: state})
	return dir, errors.Join(err, saveErr)
}

type ui struct {
	width, height, scroll int
	colors                palette
	o                     options
	field                 int
	running               bool
	note                  string
	latest                update
	events                chan update
	cancel                context.CancelFunc
	c                     *client
	ctx                   context.Context
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
			m.note = ""
			m.field = (m.field + 2) % 3
		case "down", "tab":
			m.note = ""
			m.field = (m.field + 1) % 3
		case "enter":
			if m.field < 2 {
				m.note = ""
				m.field++
				return m, nil
			}
			if err := m.o.validate(); err != nil {
				m.note = err.Error()
				switch {
				case !safeSegment(m.o.user):
					m.field = 0
				case !validDate(m.o.from):
					m.field = 1
				default:
					m.field = 2
				}
				return m, nil
			}
			ctx, cancel := context.WithCancel(m.ctx)
			m.cancel = cancel
			m.running = true
			m.note = ""
			c, o, ch := m.c, m.o, m.events
			return m, tea.Batch(m.listen, func() tea.Msg {
				var final update
				dir, err := export(ctx, c, o, func(u update) {
					final = u
					select {
					case ch <- u:
					default:
					}
				})
				final.dir, final.done, final.err = dir, true, err
				ch <- final
				return nil
			})
		case "backspace":
			m.note = ""
			r := []rune(*fields[m.field])
			if len(r) > 0 {
				*fields[m.field] = string(r[:len(r)-1])
			}
		default:
			if x.Text != "" {
				m.note = ""
				*fields[m.field] += cleanText(x.Text)
			}
		}
	case tea.PasteMsg:
		if !m.running && !m.latest.done {
			m.note = ""
			fields := []*string{&m.o.user, &m.o.from, &m.o.to}
			*fields[m.field] += cleanText(x.Content)
		}
	}
	return m, nil
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
		m := &ui{colors: loadPalette(ctx), o: o, c: c, ctx: ctx, events: make(chan update, 1)}
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
