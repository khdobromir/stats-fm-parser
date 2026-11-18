package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

func Run() error {
	var o options
	flag.StringVar(&o.User, "user", "", "stats.fm username (batch mode)")
	flag.StringVar(&o.Period, "period", "", "export period: week, weeks, months, ytd, custom")
	flag.StringVar(&o.From, "from", "", "first UTC date, YYYY-MM-DD")
	flag.StringVar(&o.To, "to", "", "last UTC date, YYYY-MM-DD")
	flag.StringVar(&o.Out, "out", "exports", "parent directory for new exports")
	flag.StringVar(&o.spotifyLibrary, "spotify-library", "", "export Liked Songs from a Spotify Account data ZIP or YourLibrary.json")
	flag.StringVar(&o.Resume, "resume", "", "resume a saved export directory")
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
	var dir string
	var err error
	if o.spotifyLibrary != "" || o.User != "" || o.Resume != "" || o.Period != "" {
		last := time.Time{}
		dir, err = export(ctx, o, func(u update) {
			if u.file != "" {
				fmt.Fprintf(os.Stderr, "Сохранено треков: %d\n%s\n", u.Progress.Records, u.file)
				return
			}
			if u.Progress.Path == "" || time.Since(last) > 2*time.Second {
				fmt.Fprintf(os.Stderr, "%s  %s  %d records\n", u.Dir, u.Progress.Path, u.Progress.Records)
				last = time.Now()
			}
		})
	} else {
		m := &ui{colors: loadPalette(ctx), o: o, ctx: ctx, events: make(chan update, 1)}
		_, err = tea.NewProgram(m).Run()
		if m.cancel != nil {
			m.cancel()
		}
		if err == nil {
			dir, err = m.latest.Dir, m.latest.err
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
