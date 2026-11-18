package statsfm

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type config struct {
	Version int         `json:"version,omitempty"`
	Profile profile     `json:"profile"`
	Periods []string    `json:"periods"`
	From    string      `json:"from,omitempty"`
	To      string      `json:"to,omitempty"`
	Started time.Time   `json:"started_at"`
	Window  *dateWindow `json:"window,omitempty"`
}

func (c config) version() int {
	return max(c.Version, 1)
}

func (c config) historyQuery() query {
	q := query{Path: "streams"}
	if c.Window != nil {
		q.Period = c.Periods[0]
	}
	return q
}

func (c config) historyEnd() int64 {
	if c.Window != nil {
		return min(c.Started.UnixMilli(), c.Window.Before)
	}
	return c.Started.UnixMilli()
}

func dateBounds(from, to string) (time.Time, time.Time, error) {
	a, e := time.Parse(time.DateOnly, from)
	b, f := time.Parse(time.DateOnly, to)
	if e != nil || f != nil || b.Before(a) {
		return a, b, errors.New("укажите обе даты в формате YYYY-MM-DD, начиная с более ранней")
	}
	return a, b.AddDate(0, 0, 1).Add(-time.Millisecond), nil
}

type query struct {
	Path   string `json:"method"`
	Period string `json:"period,omitempty"`
	Order  string `json:"requested_order,omitempty"`
	IDs    string `json:"ids,omitempty"`
}

func (q query) file() string {
	switch {
	case strings.HasPrefix(q.Path, "top/"):
		return "rankings"
	case strings.HasPrefix(q.Path, "recaps/"):
		return "recaps"
	case strings.HasPrefix(q.Path, "streams") && !strings.Contains(q.Path, "/stats"):
		return "streams"
	default:
		return "statistics"
	}
}
func (q query) global() bool {
	kind, _, _ := strings.Cut(q.Path, "/")
	return kind == "tracks" || kind == "albums" || kind == "artists"
}

func (q query) metadata() bool {
	return q.global() && strings.Count(q.Path, "/") == 1
}

func (q query) outputFile(c config) string {
	if c.version() >= 2 {
		if q.metadata() {
			return "metadata"
		}
		if q.file() == "streams" && q.Path != "streams" {
			return "statistics"
		}
	}
	return q.file()
}

func (q query) history() bool {
	return q.file() == "streams" && q.Path != "streams/current" && q.Path != "streams/recent"
}
func (q query) paged() bool {
	return q.file() == "rankings" || q.history() || q.Path == "streams/recent"
}
func (q query) params(c config) url.Values {
	v := url.Values{}
	if q.global() {
		if q.IDs != "" {
			v.Set("ids", q.IDs)
		}
		return v
	}
	if q.Path == "user" {
		return v
	}
	if c.Window != nil {
		v.Set("after", strconv.FormatInt(c.Window.After, 10))
		v.Set("before", strconv.FormatInt(c.Window.Before, 10))
	} else if q.Period == CustomPeriod {
		a, b, _ := dateBounds(c.From, c.To)
		v.Set("after", strconv.FormatInt(a.UnixMilli(), 10))
		v.Set("before", strconv.FormatInt(b.UnixMilli(), 10))
	} else if q.Period != "" {
		v.Set("range", q.Period)
	}
	if q.Order != "" && q.Order != "default" {
		v.Set("orderBy", q.Order)
	}
	if strings.HasSuffix(q.Path, "/dates") || strings.HasSuffix(q.Path, "/per-day") {
		v.Set("timeZone", "UTC")
	}
	return v
}

type entity struct{ kind, id string }

func queries(c config, e entity) []query {
	var out []query
	add := func(path string) { out = append(out, query{Path: path}) }
	streamPath := "streams"
	var tops []string
	switch e.kind {
	case "":
		if c.version() >= 2 && c.Window == nil {
			for _, path := range []string{"profile", "privacy", "friends", "friends/count", "achievements/completed", "achievements/uncompleted"} {
				add(path)
			}
		}
		tops = []string{"tracks", "artists", "albums", "genres"}

	case "artists":
		tops = []string{"artists/" + e.id + "/tracks", "artists/" + e.id + "/albums"}
		if c.Window == nil {
			add("recaps/artist/" + e.id)
		}
	case "albums":
		tops = []string{"albums/" + e.id + "/tracks"}
	}
	if e.kind != "" {
		streamPath += "/" + e.kind + "/" + e.id
		if c.version() >= 2 && c.Window == nil {
			add(e.kind + "/" + e.id + "/top/listeners/place")
		}
	}
	for _, p := range c.Periods {
		for _, s := range tops {
			for _, order := range c.Profile.Orders {
				if c.Window != nil && order != "COUNT" && order != "TIME" {
					continue
				}
				if (order == "SPOTIFY" || order == "APPLEMUSIC") && (p == "custom" || p == "today" || p == "days") {
					continue
				}
				out = append(out, query{Path: "top/" + s, Period: p, Order: order})
			}
		}
		for _, suffix := range []string{"/stats", "/stats/per-day", "/stats/dates"} {
			out = append(out, query{Path: streamPath + suffix, Period: p})
		}
	}
	if e.kind == "" && c.Window == nil {
		for _, s := range []string{"streams/current", "streams/recent", "records/artists", "recaps/weekly", "recaps/monthly", "recaps/seasonal", "recaps/yearly", "recaps/artist", "recaps/time-capsule"} {
			add(s)
		}
	}
	if c.version() >= 2 && e.kind == "" {
		return append([]query{c.historyQuery()}, out...)
	}
	if c.Window != nil {
		out = append(out, query{Path: streamPath, Period: c.Periods[0]})
	} else {
		add(streamPath)
	}
	return out
}
func entityID(v any) string {
	s := fmt.Sprint(v)
	if _, err := strconv.ParseUint(s, 10, 64); err != nil {
		return ""
	}
	return s
}

// The API embeds entities in rankings and recaps, and uses bare IDs in streams.
func discover(v any, kind string, add func(entity)) {
	switch x := v.(type) {
	case []any:
		for _, child := range x {
			discover(child, kind, add)
		}
	case object:
		if kind != "" {
			if id := entityID(x["id"]); id != "" {
				add(entity{kind, id})
			}
		}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			child := x[k]
			next := ""
			switch k {
			case "artist", "artists":
				next = "artists"
			case "album", "albums":
				next = "albums"
			case "track", "tracks":
				next = "tracks"
			case "trackId", "albumId", "artistId", "artistIds":
				ids, ok := child.([]any)
				if !ok {
					ids = []any{child}
				}
				t := strings.Split(k, "Id")[0] + "s"
				for _, val := range ids {
					if id := entityID(val); id != "" {
						add(entity{t, id})
					}
				}
			}
			discover(child, next, add)
		}
	}
}
