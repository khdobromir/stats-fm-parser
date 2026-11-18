package statsfm

import (
	"encoding/json"
	"slices"
	"strings"
)

type reportSource struct {
	File   string `json:"file"`
	Query  query  `json:"query"`
	Field  string `json:"match_field,omitempty"`
	After  *int64 `json:"after,omitempty"`
	Before int64  `json:"before"`
}

var streamEntityFields = map[string]string{
	"tracks": "trackId", "albums": "albumId", "artists": "artistIds",
}

func streamID(row any) string {
	o, _ := row.(object)
	id, _ := o["id"].(string)
	return id
}

func validStreamEntity(value any) bool {
	id := entityID(value)
	return id != "" && strings.TrimLeft(id, "0") != ""
}

func (i *index) checkStreamFields(row any) {
	o, _ := row.(object)
	if streamID(row) == "" {
		i.missing["id"] = true
	}
	for _, field := range []string{"trackId", "albumId"} {
		if !validStreamEntity(o[field]) {
			i.missing[field] = true
		}
	}
	artists, ok := o["artistIds"].([]any)
	if !ok || len(artists) == 0 {
		i.missing["artistIds"] = true
	}
	for _, artist := range artists {
		if !validStreamEntity(artist) {
			i.missing["artistIds"] = true
		}
	}
	played, _ := o["playedMs"].(json.Number)
	if ms, err := played.Int64(); err != nil || ms < 0 {
		i.missing["playedMs"] = true
	}
}

func (i *index) reconstruct(cfg config, q query) *reportSource {
	if cfg.version() == 1 || !strings.HasPrefix(q.Path, "streams/") {
		return nil
	}
	history := cfg.historyQuery()
	r := i.results[history]
	if r == nil || r.Status != "complete" || r.Count == 0 || i.missing["id"] {
		return nil
	}

	base, report, summary := strings.Cut(q.Path, "/stats/")
	source := &reportSource{File: "streams.jsonl", Query: history, Before: cfg.historyEnd()}
	if cfg.Window != nil {
		source.After = &cfg.Window.After
	}
	if summary {
		if (report != "per-day" && report != "dates") || i.missing["playedMs"] {
			return nil
		}
		switch {
		case cfg.Window != nil && q.Period == history.Period:
			if cfg.Window.Before > source.Before {
				return nil
			}
		case cfg.Window == nil && q.Period == CustomPeriod:
			after, before, err := dateBounds(cfg.From, cfg.To)
			if err != nil || before.UnixMilli() > source.Before {
				return nil
			}
			start := after.UnixMilli()
			source.After, source.Before = &start, before.UnixMilli()
		default:
			return nil
		}
	} else if q.Period != history.Period || cfg.Window != nil && cfg.Window.Before > source.Before {
		return nil
	}

	if summary && base == "streams" {
		return source
	}
	kind, id, _ := strings.Cut(strings.TrimPrefix(base, "streams/"), "/")
	source.Field = streamEntityFields[kind]
	if source.Field == "" || !validStreamEntity(id) || i.missing[source.Field] {
		return nil
	}
	return source
}

func metadataRows(q query, rows []any) []any {
	ids := q.IDs
	if ids == "" {
		_, ids, _ = strings.Cut(q.Path, "/")
	}
	needed := map[string]bool{}
	for id := range strings.SplitSeq(ids, ",") {
		needed[id] = true
	}
	var out []any
	for _, row := range rows {
		o, _ := row.(object)
		id := entityID(o["id"])
		if name, _ := o["name"].(string); needed[id] && name != "" {
			out = append(out, row)
			delete(needed, id)
		}
	}
	return out
}

// A ranking item can supply the same catalog object when it has the full v1 projection.
var catalogFields = map[string][]string{
	"tracks":  {"albums", "appleMusicPreview", "artists", "durationMs", "explicit", "externalIds", "name", "spotifyPopularity", "spotifyPreview"},
	"albums":  {"artists", "externalIds", "genres", "image", "label", "name", "releaseDate", "spotifyPopularity", "totalTracks", "type"},
	"artists": {"externalIds", "followers", "genres", "image", "name", "spotifyPopularity"},
}

func (i *index) noteRankingMetadata(q query, row any) {
	kind := strings.TrimPrefix(q.Path, "top/")
	o, _ := row.(object)
	item, _ := o[strings.TrimSuffix(kind, "s")].(object)
	id := entityID(item["id"])
	name, _ := item["name"].(string)
	if id == "" || name == "" {
		return
	}
	for _, field := range catalogFields[kind] {
		if _, ok := item[field]; !ok {
			return
		}
	}
	i.metadata[entity{kind, id}] = true
}

func (i *index) metadataBatches() []query {
	attempted := map[entity]bool{}
	for q := range i.results {
		if !q.metadata() || q.IDs == "" {
			continue
		}
		kind, _, _ := strings.Cut(q.Path, "/")
		for id := range strings.SplitSeq(q.IDs, ",") {
			attempted[entity{kind, id}] = true
		}
	}
	var batches []query
	for _, kind := range []string{"tracks", "albums", "artists"} {
		var ids []string
		for _, e := range i.entities {
			if e.kind == kind && !i.metadata[e] && !attempted[e] {
				ids = append(ids, e.id)
			}
		}
		slices.Sort(ids)
		for batch := range slices.Chunk(ids, 50) {
			batches = append(batches, query{Path: kind + "/list", IDs: strings.Join(batch, ",")})
		}
	}
	return batches
}
