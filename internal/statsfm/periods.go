package statsfm

import "time"

const CustomPeriod = "custom"

type dateWindow struct {
	After  int64 `json:"after"`
	Before int64 `json:"before"`
}

func (o Options) exportConfig(p profile, started time.Time) config {
	cfg := config{Version: 2, Profile: p, Started: started.UTC(), From: o.From, To: o.To}
	if o.Period == "" {
		cfg.Periods = []string{"today", "days", "weeks", "months", "lifetime"}
		if o.From != "" {
			cfg.Periods = append(cfg.Periods, CustomPeriod)
		}
		return cfg
	}

	cfg.Periods = []string{o.Period}
	end := cfg.Started
	var start time.Time
	switch o.Period {
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
	case CustomPeriod:
		start, end, _ = dateBounds(o.From, o.To)
	}
	cfg.Window = &dateWindow{After: start.UnixMilli(), Before: end.UnixMilli()}
	return cfg
}
