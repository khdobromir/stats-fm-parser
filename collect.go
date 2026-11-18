package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type page struct {
	FetchedAt time.Time `json:"fetched_at"`
	Query     query     `json:"query"`
	Rows      []any     `json:"rows,omitempty"`
	Offset    int       `json:"next_offset,omitempty"`
	Before    int64     `json:"next_before,omitempty"`
	Status    string    `json:"status"`
	Note      string    `json:"note,omitempty"`
}
type result struct {
	Query  query  `json:"query"`
	Count  int    `json:"count"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
	Offset int    `json:"next_offset,omitempty"`
	Before int64  `json:"next_before,omitempty"`
	seen   map[string]bool
}
type index struct {
	results    map[query]*result
	entities   []entity
	discovered map[entity]bool
	count      int
}

func newIndex() *index { return &index{results: map[query]*result{}, discovered: map[entity]bool{}} }
func key(q query, row any) string {
	o, _ := row.(object)
	if q.file() == "rankings" {
		for _, k := range []string{"track", "artist", "album", "genre"} {
			v, _ := o[k].(object)
			if id := entityID(v["id"]); id != "" {
				return id
			}
			if s, ok := v["tag"].(string); ok {
				return s
			}
		}
	}
	if id, ok := o["id"].(string); ok {
		return id
	}
	b, _ := json.Marshal(row)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func (i *index) apply(p page) {
	r := i.results[p.Query]
	if r == nil {
		r = &result{Query: p.Query}
		i.results[p.Query] = r
	}
	r.Count += len(p.Rows)
	r.Offset = p.Offset
	r.Before = p.Before
	r.Status = p.Status
	r.Note = p.Note
	i.count += len(p.Rows)
	if p.Status == "running" && (r.seen == nil || p.Query.history()) {
		r.seen = map[string]bool{}
	}
	for _, row := range p.Rows {
		if p.Status == "running" && (!p.Query.history() || streamMillis(row) == p.Before) {
			r.seen[key(p.Query, row)] = true
		}
		discover(row, "", func(e entity) {
			if !i.discovered[e] {
				i.discovered[e] = true
				i.entities = append(i.entities, e)
			}
		})
	}
	if p.Status != "running" {
		r.seen = nil
	}
}
func rowsFor(q query, v any) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	o, ok := v.(object)
	if ok {
		if items, exists := o["items"]; exists {
			v = items
		} else if item, exists := o["item"]; exists {
			v = item
		} else if q.file() != "recaps" {
			return nil, errors.New("missing item/items envelope")
		}
	}
	if v == nil {
		return nil, nil
	}
	if rows, ok := v.([]any); ok {
		return rows, nil
	}
	if q.paged() {
		return nil, errors.New("expected an array of items")
	}
	if _, ok := v.(object); !ok {
		return nil, errors.New("expected a report object")
	}
	return []any{v}, nil
}

func streamMillis(row any) int64 {
	o, _ := row.(object)
	stamp, _ := o["endTime"].(string)
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

const pageSize = 100

func (c *client) fetch(ctx context.Context, cfg config, q query, r *result) (page, error) {
	p := page{Query: q, Status: "complete", Offset: r.Offset, Before: r.Before}
	v := q.params(cfg)
	if q.paged() {
		v.Set("limit", strconv.Itoa(pageSize))
		if q.history() {
			before := r.Before
			if before == 0 {
				before = cfg.Started.UnixMilli()
			}
			v.Set("before", strconv.FormatInt(before, 10))
			v.Set("order", "desc")
		} else {
			v.Set("offset", strconv.Itoa(r.Offset))
		}
	}
	body, err := c.get(ctx, url.PathEscape(cfg.Profile.ID)+"/"+q.Path, v)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.code != 429 {
			p.Status = "unavailable"
			if ae.code >= 500 {
				p.Status = "failed"
			}
			if ae.code == 401 || ae.code == 403 {
				p.Status = "private"
			}
			if r.Count > 0 {
				p.Status = "partial"
			}
			p.Note = ae.Error()
			return p, nil
		}
		if errors.Is(err, errFormat) {
			p.Status = "partial"
			p.Note = err.Error()
			return p, nil
		}
		return p, err
	}
	rows, err := rowsFor(q, body)
	if err != nil {
		p.Status = "partial"
		p.Note = err.Error()
		return p, nil
	}
	if !q.paged() {
		p.Rows = rows
		return p, nil
	}
	if len(rows) == 0 {
		return p, nil
	}
	oldest := int64(0)
	for _, row := range rows {
		if q.history() {
			ms := streamMillis(row)
			if ms <= 0 {
				p.Status = "partial"
				p.Note = "missing or invalid endTime"
			} else {
				if (r.Before != 0 && ms > r.Before) || ms > cfg.Started.UnixMilli() {
					p.Status = "partial"
					p.Note = "API ignored the time cursor"
					continue
				}
				if oldest == 0 || ms < oldest {
					oldest = ms
				}
			}
		}
		if !r.seen[key(q, row)] {
			p.Rows = append(p.Rows, row)
		}
	}
	if p.Status == "partial" {
		return p, nil
	}
	if q.history() {
		// before is inclusive. Re-read the boundary millisecond to retain tied streams.
		if oldest == r.Before && len(rows) < pageSize {
			oldest--
		}
		if r.Before != 0 && oldest >= r.Before {
			p.Status = "partial"
			p.Note = "time cursor stopped advancing"
			return p, nil
		}
		p.Before = oldest
	} else {
		if len(p.Rows) == 0 {
			p.Status = "partial"
			p.Note = "API repeated the page or reached its limit"
			return p, nil
		}
		p.Offset += len(rows)
	}
	p.Status = "running"
	return p, nil
}

type progress struct {
	Path                 string
	Records, Done, Total int
}

func collect(ctx context.Context, c *client, s *store, idx *index, notify func(progress)) error {
	run := func(e entity, done int) error {
		for _, q := range queries(s.cfg, e) {
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				r := idx.results[q]
				if r == nil {
					r = &result{Query: q}
				} else if r.Status != "running" {
					break
				}
				notify(progress{q.Path, idx.count, done, len(idx.entities)})
				p, err := c.fetch(ctx, s.cfg, q, r)
				if err != nil {
					return fmt.Errorf("%s: %w", q.Path, err)
				}
				if err = s.append(p); err != nil {
					return err
				}
				idx.apply(p)
			}
		}
		return nil
	}
	if err := run(entity{}, 0); err != nil {
		return err
	}
	for n := 0; n < len(idx.entities); n++ {
		if err := run(idx.entities[n], n); err != nil {
			return err
		}
	}
	return nil
}
func compactRanking(q query, row any) object {
	o, _ := row.(object)
	kind := strings.TrimSuffix(strings.TrimPrefix(q.Path, "top/"), "s")
	v, _ := o[kind].(object)
	out := object{"position": o["position"], "streams": o["streams"], "played_ms": o["playedMs"], "id": v["id"], "name": v["name"]}
	if kind == "genre" {
		out["name"] = v["tag"]
		out["id"] = v["tag"]
	}
	if artists, ok := v["artists"].([]any); ok {
		names := []string{}
		for _, item := range artists {
			artist, _ := item.(object)
			if name, ok := artist["name"].(string); ok {
				names = append(names, name)
			}
		}
		out["artists"] = names
	}
	return out
}
