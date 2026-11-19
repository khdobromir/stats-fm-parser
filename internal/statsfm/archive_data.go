package statsfm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type archiveData struct {
	streams  map[string]page
	order    []string
	metadata map[entity]page
	user     *page
}

func newArchiveData() *archiveData {
	return &archiveData{streams: map[string]page{}, metadata: map[entity]page{}}
}

func (d *archiveData) add(p page, cfg config) error {
	for _, row := range p.Rows {
		entry := page{Query: p.Query, FetchedAt: p.FetchedAt, Rows: []any{row}}
		switch {
		case p.Query.Path == "streams":
			id, ms := streamID(row), streamMillis(row)
			if id == "" || ms <= 0 {
				return errors.New("прослушивание без ID или корректной метки времени")
			}
			item, _ := row.(object)
			if user, _ := item["userId"].(string); user != "" && user != cfg.Profile.ID {
				return errors.New("прослушивание принадлежит другому профилю")
			}
			if w := cfg.Window; w != nil && (ms < w.After || ms > w.Before) {
				continue
			}
			entry.Query = cfg.historyQuery()
			if _, exists := d.streams[id]; !exists {
				d.order = append(d.order, id)
			}
			d.streams[id] = entry
		case p.Query.metadata():
			kind, _, _ := strings.Cut(p.Query.Path, "/")
			item, _ := row.(object)
			if id := entityID(item["id"]); id != "" {
				d.metadata[entity{kind, id}] = entry
			}
		case p.Query.Path == "user":
			d.user = &entry
		}
	}
	return nil
}

func (d *archiveData) load(dir string, cfg config) error {
	for _, name := range []string{"streams", "metadata", "statistics"} {
		path := filepath.Join(dir, name+".jsonl")
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(f)
		dec.UseNumber()
		for {
			var row exportRow
			err = dec.Decode(&row)
			if err != nil {
				break
			}
			if err = d.add(page{Query: row.query, FetchedAt: row.FetchedAt, Rows: []any{row.Data}}, cfg); err != nil {
				break
			}
		}
		f.Close()
		if err != io.EOF {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func (d *archiveData) merge(s *store) error {
	delta := newArchiveData()
	q := s.cfg.historyQuery()
	if err := s.replay(func(p page) error {
		if p.Query == q && p.Status == "retry" {
			clear(delta.streams)
			delta.order = nil
			return nil
		}
		if p.Status == "partial" && p.Query == q {
			return nil
		}
		return delta.add(p, s.cfg)
	}); err != nil {
		return err
	}
	for _, id := range delta.order {
		if err := d.add(delta.streams[id], s.cfg); err != nil {
			return err
		}
	}
	for e, p := range delta.metadata {
		d.metadata[e] = p
	}
	if delta.user != nil {
		d.user = delta.user
	}
	return nil
}

func (d *archiveData) rows(visit func(page) error) error {
	slices.SortStableFunc(d.order, func(a, b string) int {
		left, right := streamMillis(d.streams[a].Rows[0]), streamMillis(d.streams[b].Rows[0])
		if left > right {
			return -1
		}
		if left < right {
			return 1
		}
		return 0
	})
	for _, id := range d.order {
		if err := visit(d.streams[id]); err != nil {
			return err
		}
	}
	entities := make([]entity, 0, len(d.metadata))
	for e := range d.metadata {
		entities = append(entities, e)
	}
	slices.SortFunc(entities, func(a, b entity) int {
		if order := strings.Compare(a.kind, b.kind); order != 0 {
			return order
		}
		return strings.Compare(a.id, b.id)
	})
	for _, e := range entities {
		if err := visit(d.metadata[e]); err != nil {
			return err
		}
	}
	if d.user != nil {
		return visit(*d.user)
	}
	return nil
}
