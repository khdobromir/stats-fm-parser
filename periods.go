package main

import "time"

const customPeriod = "custom"

var periodChoices = [...]struct{ key, label string }{
	{"", "Все стандартные"},
	{"week", "За неделю"},
	{"weeks", "За 4 недели"},
	{"months", "За 6 месяцев"},
	{"ytd", "За текущий год"},
	{customPeriod, "Свои даты"},
}

type dateWindow struct {
	After  int64 `json:"after"`
	Before int64 `json:"before"`
}

func (o options) exportConfig(p profile, started time.Time) config {
	cfg := config{Profile: p, Started: started.UTC(), From: o.from, To: o.to}
	if o.period == "" {
		cfg.Periods = []string{"today", "days", "weeks", "months", "lifetime"}
		if o.from != "" {
			cfg.Periods = append(cfg.Periods, customPeriod)
		}
		return cfg
	}

	cfg.Periods = []string{o.period}
	end := cfg.Started
	var start time.Time
	switch o.period {
	case "week":
		start = end.AddDate(0, 0, -7)
	case "weeks":
		start = end.AddDate(0, 0, -28)
	case "months":
		month := time.Date(end.Year(), end.Month()-6, 1, end.Hour(), end.Minute(), end.Second(), end.Nanosecond(), time.UTC)
		lastDay := month.AddDate(0, 1, -1).Day()
		start = month.AddDate(0, 0, min(end.Day(), lastDay)-1)
	case "ytd":
		start = time.Date(end.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
	case customPeriod:
		start, end, _ = dateBounds(o.from, o.to)
	}
	cfg.Window = &dateWindow{After: start.UnixMilli(), Before: end.UnixMilli()}
	return cfg
}
