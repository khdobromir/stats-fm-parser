package statsfm

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
	FetchedAt time.Time       `json:"fetched_at"`
	Query     query           `json:"query"`
	Rows      []any           `json:"rows,omitempty"`
	Offset    int             `json:"next_offset,omitempty"`
	Before    int64           `json:"next_before,omitempty"`
	Status    string          `json:"status"`
	Note      string          `json:"note,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
	Source    *reportSource   `json:"source,omitempty"`
}
type result struct {
	Query  query         `json:"query"`
	Count  int           `json:"count"`
	Status string        `json:"status"`
	Note   string        `json:"note,omitempty"`
	Offset int           `json:"next_offset,omitempty"`
	Before int64         `json:"next_before,omitempty"`
	Source *reportSource `json:"source,omitempty"`
	seen   map[string]bool
}
type index struct {
	results    map[query]*result
	entities   []entity
	discovered map[entity]bool
	missing    map[string]bool
	metadata   map[entity]bool
	count      int
}

func newIndex() *index {
	return &index{results: map[query]*result{}, discovered: map[entity]bool{}, metadata: map[entity]bool{}, missing: map[string]bool{}}
}
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
	if id, ok := o["id"].(string); ok && id != "" {
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
	r.Source = p.Source
	i.count += len(p.Rows)
	if p.Status == "running" && (r.seen == nil || p.Query.history()) {
		r.seen = map[string]bool{}
	}
	for _, row := range p.Rows {
		if p.Status == "running" && (!p.Query.history() || streamMillis(row) == p.Before) {
			r.seen[key(p.Query, row)] = true
		}
		if p.Query.metadata() {
			kind, _, _ := strings.Cut(p.Query.Path, "/")
			item, _ := row.(object)
			if id := entityID(item["id"]); id != "" {
				i.metadata[entity{kind, id}] = true
			}
			continue
		}
		if p.Response != nil {
			switch p.Query.Path {
			case "streams":
				i.checkStreamFields(row)
			case "top/tracks", "top/albums", "top/artists":
				i.noteRankingMetadata(p.Query, row)
			}
		}
		i.discoverRow(row)
	}
	if p.Status != "running" {
		r.seen = nil
	}
}
func (i *index) discoverRow(row any) {
	discover(row, "", func(e entity) {
		if !i.discovered[e] {
			i.discovered[e] = true
			i.entities = append(i.entities, e)
		}
	})
}

func rowsFor(q query, v any, cfg config) ([]any, error) {
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
	if _, ok := v.(object); !ok && !(cfg.version() >= 2 && (q.Path == "friends/count" || q.global() && !q.metadata())) {
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

func (q query) pageLimit(cfg config) int {
	if cfg.version() >= 3 {
		switch q.Path {
		case "streams":
			return 1000
		case "top/tracks":
			return 500
		}
	}
	return pageSize
}

func (c *client) fetch(ctx context.Context, cfg config, q query, r *result) (page, error) {
	p := page{Query: q, Status: "complete", Offset: r.Offset, Before: r.Before}
	v := q.params(cfg)
	before := cfg.historyEnd()
	window := cfg.scanWindow()
	paged := q.paged() && (cfg.version() == 1 || q.Path != "streams/recent")

	if paged {
		v.Set("limit", strconv.Itoa(q.pageLimit(cfg)))
		if q.history() {
			if r.Before != 0 {
				before = min(before, r.Before)
			}
			if window != nil && before < window.After {
				if cfg.version() >= 2 && r.Count == 0 {
					p.Status = "empty"
				}
				return p, nil
			}
			v.Set("before", strconv.FormatInt(before, 10))
			v.Set("order", "desc")
		} else {
			v.Set("offset", strconv.Itoa(r.Offset))
		}
	}
	target := c.base + url.PathEscape(cfg.Profile.ID)
	switch {
	case q.Path == "user":
	case q.metadata():
		target = strings.TrimSuffix(c.base, "users/") + q.Path
	case q.global():
		target = strings.TrimSuffix(c.base, "users/") + q.Path + "/" + url.PathEscape(cfg.Profile.ID)
	default:
		target += "/" + q.Path
	}
	raw, err := c.get(ctx, target, v)
	if cfg.version() >= 2 {
		p.Response = raw
	}
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
	rows, err := rowsFor(q, decodeResponse(raw), cfg)
	if err != nil {
		p.Status = "partial"
		p.Note = err.Error()
		return p, nil
	}
	if q.metadata() {
		p.Rows = metadataRows(q, rows)
		want := 1
		if q.IDs != "" {
			want = strings.Count(q.IDs, ",") + 1
		}
		if len(p.Rows) < want {
			p.Status = "partial"
			p.Note = "catalog response did not contain every requested ID"
		}
		return p, nil
	}
	if len(rows) == 0 {
		if cfg.version() >= 2 && r.Count == 0 {
			p.Status = "empty"
		}
		return p, nil
	}
	if !paged {
		p.Rows = rows
		return p, nil
	}
	oldest := int64(0)
	previous := before
	reachedStart := false
	var pageIDs map[string]bool
	if cfg.version() >= 2 && q.history() {
		pageIDs = map[string]bool{}
	}
	for _, row := range rows {
		if q.history() {
			if cfg.Mode == HistoryMode && streamID(row) == "" {
				p.Status = "partial"
				p.Note = "missing stream ID"
			}
			ms := streamMillis(row)
			if ms <= 0 {
				p.Status = "partial"
				p.Note = "missing or invalid endTime"
				if window != nil {
					continue
				}
			} else {
				if ms > before {
					p.Status = "partial"
					p.Note = "API ignored the time cursor"
					continue
				}
				if cfg.version() >= 2 && ms > previous {
					p.Status = "partial"
					p.Note = "API returned history out of time order"
				}
				previous = ms
				if window != nil && ms < window.After {
					reachedStart = true
					continue
				}
				if oldest == 0 || ms < oldest {
					oldest = ms
				}
			}
		}
		id := key(q, row)
		if !r.seen[id] && !pageIDs[id] {
			p.Rows = append(p.Rows, row)
			if pageIDs != nil && streamID(row) != "" {
				pageIDs[id] = true
			}
		}
	}
	if p.Status == "partial" {
		return p, nil
	}
	if reachedStart {
		if cfg.version() >= 2 && r.Count+len(p.Rows) == 0 {
			p.Status = "empty"
		}
		return p, nil
	}
	if q.history() {
		boundary := r.Before
		if cfg.version() >= 2 {
			boundary = before
		}
		// A server-clamped page can look short at the enlarged limit. At a stuck
		// boundary, only advance if it fits below the verified legacy limit.
		if boundary != 0 && oldest == boundary {
			if len(rows) >= pageSize {
				p.Status = "partial"
				p.Note = "time cursor stopped advancing"
				return p, nil
			}
			oldest--
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

type Progress struct {
	Path, Period, Order  string
	Records, Done, Total int
	Streams              int
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
