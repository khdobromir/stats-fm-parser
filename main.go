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
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

type options struct{ user, period, from, to, out, resume, spotifyLibrary string }

func (o options) validate() error {
	if o.spotifyLibrary != "" {
		if o.user != "" || o.period != "" || o.from != "" || o.to != "" || o.resume != "" {
			return errors.New("-spotify-library нельзя сочетать с -user, -period, -from, -to или -resume")
		}
		return nil
	}
	if o.resume != "" {
		if o.user != "" || o.period != "" || o.from != "" || o.to != "" {
			return errors.New("-resume uses the original username and dates")
		}
		return nil
	}
	if !safeSegment(o.user) {
		return errors.New("введите username из профиля stats.fm")
	}
	switch o.period {
	case "", customPeriod:
	case "week", "weeks", "months", "ytd":
		if o.from != "" || o.to != "" {
			return errors.New("-from and -to cannot be combined with this preset")
		}
	default:
		return fmt.Errorf("неизвестный период %q", o.period)
	}
	if o.period == customPeriod || o.from != "" || o.to != "" {
		_, _, err := dateBounds(o.from, o.to)
		return err
	}
	return nil
}

type update struct {
	state string
	file  string
	dir   string
	p     progress
	done  bool
	err   error
}

func export(ctx context.Context, c *client, o options, report func(update)) (string, error) {
	if err := o.validate(); err != nil {
		return "", err
	}
	if o.spotifyLibrary != "" {
		file, count, err := exportSpotify(ctx, o.spotifyLibrary, o.out)
		if err != nil {
			return "", err
		}
		dir := filepath.Dir(file)
		report(update{dir: dir, file: file, state: "finished", p: progress{Records: count}})
		return dir, nil
	}
	dir := o.resume
	var cfg *config
	if dir == "" {
		p, err := c.profile(ctx, o.user)
		if err != nil {
			return "", err
		}
		selected := o.exportConfig(p, time.Now())
		cfg = &selected
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
	field, selected       int
	running               bool
	spotify               bool
	libraryPath           string
	note                  string
	latest                update
	events                chan update
	cancel                context.CancelFunc
	c                     *client
	ctx                   context.Context
}

func (m *ui) lastField() int {
	if m.o.period == customPeriod {
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
			m.note = ""
			return m, nil
		}
		if m.spotify {
			switch x.String() {
			case "enter":
				if strings.TrimSpace(m.libraryPath) == "" {
					m.note = "Укажите путь к ZIP или YourLibrary.json"
					return m, nil
				}
				return m, m.start(options{spotifyLibrary: m.libraryPath, out: m.o.out})
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
		fields := []*string{&m.o.user, nil, &m.o.from, &m.o.to}
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
				m.o.period = periodChoices[m.selected].key
				m.note = ""
			}
		case "enter":
			if m.field < m.lastField() {
				m.note = ""
				m.field++
				return m, nil
			}
			o := m.o
			if o.period != customPeriod {
				o.from, o.to = "", ""
			}
			if err := o.validate(); err != nil {
				m.note = err.Error()
				switch {
				case !safeSegment(m.o.user):
					m.field = 0
				case !validDate(m.o.from):
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
		if !m.running && !m.latest.done && m.spotify {
			m.libraryPath += cleanText(x.Content)
			m.note = ""
		} else if !m.running && !m.latest.done && m.field != 1 {
			m.note = ""
			fields := []*string{&m.o.user, nil, &m.o.from, &m.o.to}
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
	c, ch := m.c, m.events
	return tea.Batch(m.listen, func() tea.Msg {
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
func run() error {
	var o options
	flag.StringVar(&o.user, "user", "", "stats.fm username (batch mode)")
	flag.StringVar(&o.period, "period", "", "export period: week, weeks, months, ytd, custom")
	flag.StringVar(&o.from, "from", "", "first UTC date, YYYY-MM-DD")
	flag.StringVar(&o.to, "to", "", "last UTC date, YYYY-MM-DD")
	flag.StringVar(&o.out, "out", "exports", "parent directory for new exports")
	flag.StringVar(&o.spotifyLibrary, "spotify-library", "", "export Liked Songs from a Spotify Account data ZIP or YourLibrary.json")
	flag.StringVar(&o.resume, "resume", "", "resume a saved export directory")
	flag.Parse()
	var spotifyFlag bool
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "spotify-library" {
			spotifyFlag = true
		}
	})
	if spotifyFlag && o.spotifyLibrary == "" {
		return errors.New("укажите путь после -spotify-library")
	}
	if flag.NArg() != 0 {
		return errors.New("use -user USERNAME or run without arguments for the TUI")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &client{base: apiBase, http: &http.Client{Timeout: 45 * time.Second}, pace: 200 * time.Millisecond}
	var dir string
	var err error
	if o.spotifyLibrary != "" || o.user != "" || o.resume != "" || o.period != "" {
		last := time.Time{}
		dir, err = export(ctx, c, o, func(u update) {
			if u.file != "" {
				fmt.Fprintf(os.Stderr, "Сохранено треков: %d\n%s\n", u.p.Records, u.file)
				return
			}
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
	if dir != "" && o.spotifyLibrary == "" {
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
