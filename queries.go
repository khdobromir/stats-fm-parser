package main

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
	Profile profile   `json:"profile"`
	Periods []string  `json:"periods"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
	Started time.Time `json:"started_at"`
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
func (q query) history() bool {
	return q.file() == "streams" && q.Path != "streams/current" && q.Path != "streams/recent"
}
func (q query) paged() bool {
	return q.file() == "rankings" || q.history() || q.Path == "streams/recent"
}
func (q query) params(c config) url.Values {
	v := url.Values{}
	if q.Period == "custom" {
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
		tops = []string{"tracks", "artists", "albums", "genres"}

	case "artists":
		tops = []string{"artists/" + e.id + "/tracks", "artists/" + e.id + "/albums"}
		add("recaps/artist/" + e.id)
	case "albums":
		tops = []string{"albums/" + e.id + "/tracks"}
	}
	if e.kind != "" {
		streamPath += "/" + e.kind + "/" + e.id
	}
	for _, p := range c.Periods {
		for _, s := range tops {
			for _, order := range c.Profile.Orders {
				if (order == "SPOTIFY" || order == "APPLEMUSIC") && (p == "custom" || p == "today" || p == "days") {
					continue
				}
				out = append(out, query{"top/" + s, p, order})
			}
		}
		for _, suffix := range []string{"/stats", "/stats/per-day", "/stats/dates"} {
			out = append(out, query{Path: streamPath + suffix, Period: p})
		}
	}
	if e.kind == "" {
		for _, s := range []string{"streams/current", "streams/recent", "records/artists", "recaps/weekly", "recaps/monthly", "recaps/seasonal", "recaps/yearly", "recaps/artist", "recaps/time-capsule"} {
			add(s)
		}
	}
	add(streamPath)
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
