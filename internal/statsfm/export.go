package statsfm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"stats-fm-parser/internal/exportfs"
)

type Options struct{ User, Period, From, To, Out, Resume string }

func (o Options) Validate() error {
	if o.Resume != "" {
		if o.User != "" || o.Period != "" || o.From != "" || o.To != "" {
			return errors.New("-resume uses the original username and dates")
		}
		return nil
	}
	if err := ValidateUsername(o.User); err != nil {
		return err
	}
	switch o.Period {
	case "", CustomPeriod:
	case "week", "weeks", "months", "ytd":
		if o.From != "" || o.To != "" {
			return errors.New("-from and -to cannot be combined with this preset")
		}
	default:
		return fmt.Errorf("неизвестный период %q", o.Period)
	}
	if o.Period == CustomPeriod || o.From != "" || o.To != "" {
		_, _, err := dateBounds(o.From, o.To)
		return err
	}
	return nil
}

func ValidateUsername(user string) error {
	if !safeSegment(user) {
		return errors.New("введите username из профиля stats.fm")
	}
	return nil
}

type Update struct {
	Dir, State string
	Progress   Progress
}

func Export(ctx context.Context, o Options, report func(Update)) (string, error) {
	c := &client{base: apiBase, http: &http.Client{Timeout: 45 * time.Second}, pace: 200 * time.Millisecond}
	return c.export(ctx, o, report)
}

func (c *client) export(ctx context.Context, o Options, report func(Update)) (string, error) {
	if err := o.Validate(); err != nil {
		return "", err
	}
	dir := o.Resume
	var cfg *config
	if dir == "" {
		p, err := c.profile(ctx, o.User)
		if err != nil {
			return "", err
		}
		selected := o.exportConfig(p, time.Now())
		cfg = &selected
		if err = os.MkdirAll(o.Out, 0700); err != nil {
			return "", err
		}
		dir, err = os.MkdirTemp(o.Out, "statsfm-"+cfg.Started.Format("20060102-150405")+"-")
		if err != nil {
			return "", err
		}
	}
	s, err := openStore(dir, cfg)
	if err != nil {
		return dir, err
	}
	defer s.close()
	report(Update{Dir: dir})
	idx := newIndex()
	if err = s.replay(func(p page) error { idx.apply(p); return nil }); err != nil {
		return dir, err
	}
	if err = exportfs.WriteJSON(dir, "manifest.json", object{"version": 1, "session": s.cfg, "state": "running"}); err != nil {
		return dir, err
	}
	var last Progress
	err = collect(ctx, c, s, idx, func(p Progress) { last = p; report(Update{Dir: dir, Progress: p}) })
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
	last.Path, last.Period, last.Order = "publish", "", ""
	last.Records = idx.count
	last.Total = len(idx.entities)
	if err == nil {
		last.Done = last.Total
	}
	report(Update{Dir: dir, Progress: last})
	saveErr := s.publish(idx, state, err)
	if saveErr != nil {
		state = "save_error"
	}
	report(Update{Dir: dir, Progress: last, State: state})
	return dir, errors.Join(err, saveErr)
}
