package statsfm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"stats-fm-parser/internal/exportfs"
)

type Options struct {
	User, Period, From, To, Out, Resume      string
	Mode, Update, RefreshFrom, ImportHistory string
}

func (o Options) Validate() error {
	operations := 0
	for _, path := range []string{o.Resume, o.Update, o.ImportHistory} {
		if path != "" {
			operations++
		}
	}
	if operations > 1 {
		return errors.New("выберите одну операцию из -resume, -update и -import-history")
	}
	if o.RefreshFrom != "" {
		if o.Update == "" {
			return errors.New("-refresh-from требует -update")
		}
		if _, err := time.Parse(time.DateOnly, o.RefreshFrom); err != nil {
			return fmt.Errorf("-refresh-from: %w", err)
		}
	}
	if operations == 1 {
		if o.User != "" || o.Period != "" || o.From != "" || o.To != "" || o.Mode != "" {
			return errors.New("операция использует профиль, период и режим исходного экспорта")
		}
		return nil
	}
	if o.Mode != "" && o.Mode != "full" && o.Mode != HistoryMode {
		return fmt.Errorf("неизвестный режим %q", o.Mode)
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
	if o.ImportHistory != "" {
		return importArchive(ctx, o, report)
	}
	if o.Update != "" {
		return c.archive(ctx, o.Update, o, nil, nil, report)
	}
	if o.Resume != "" && isArchive(o.Resume) {
		return c.archive(ctx, o.Resume, o, nil, nil, report)
	}
	dir := o.Resume
	var cfg *config
	var profileResponse json.RawMessage
	if dir == "" {
		p, response, err := c.profile(ctx, o.User)
		if err != nil {
			return "", err
		}
		profileResponse = response
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
	if cfg != nil && cfg.Mode == HistoryMode {
		return c.archive(ctx, dir, o, cfg, profileResponse, report)
	}
	s, err := openStore(dir, cfg)
	if err != nil {
		return dir, err
	}
	defer s.close()
	if s.cfg.Mode == HistoryMode {
		return dir, errors.New("для -resume укажите корневой каталог архива истории")
	}
	report(Update{Dir: dir})
	idx := newIndex()
	if err = s.replay(func(p page) error { idx.apply(p); return nil }); err != nil {
		return dir, err
	}
	if profileResponse != nil {
		q := query{Path: "user"}
		p := page{Query: q, Status: "complete", Response: profileResponse}
		p.Rows, err = rowsFor(q, decodeResponse(profileResponse), s.cfg)
		if err != nil {
			return dir, err
		}
		if err = s.append(p); err != nil {
			return dir, err
		}
		idx.apply(p)
	}
	if err = exportfs.WriteJSON(dir, "manifest.json", object{"version": s.cfg.version(), "session": s.cfg, "state": "running"}); err != nil {
		return dir, err
	}
	var last Progress
	err = collect(ctx, c, s, idx, func(p Progress) { last = p; report(Update{Dir: dir, Progress: p}) })
	state := "finished"
	for _, r := range idx.results {
		if r.Status != "complete" && r.Status != "empty" && r.Status != "reconstructible" &&
			!(r.Query.metadata() && r.Query.IDs != "") {
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
