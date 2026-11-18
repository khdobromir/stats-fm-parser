package statsfm

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type playTotals struct {
	Count      int   `json:"count"`
	DurationMs int64 `json:"durationMs"`
}

func TestReconstructClosedPeriod(t *testing.T) {
	b, err := os.ReadFile("testdata/closed-period.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		After, Before time.Time
		Streams       json.RawMessage
		Reports       map[string]json.RawMessage
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}

	cfg := config{Version: 2, Started: fixture.Before.Add(time.Hour), Periods: []string{CustomPeriod},
		Window: &dateWindow{After: fixture.After.UnixMilli(), Before: fixture.Before.UnixMilli()}}
	rows, err := rowsFor(cfg.historyQuery(), decodeResponse(fixture.Streams), cfg)
	if err != nil {
		t.Fatal(err)
	}
	idx := newIndex()
	idx.apply(page{Query: cfg.historyQuery(), Status: "complete", Rows: rows, Response: fixture.Streams})
	for path, reference := range fixture.Reports {
		t.Run(path, func(t *testing.T) {
			source := idx.reconstruct(cfg, query{Path: path, Period: CustomPeriod})
			if source == nil || source.After == nil {
				t.Fatal("missing source coverage")
			}
			got, err := json.Marshal(restoreReport(path, *source, rows))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decodeResponse(got), decodeResponse(reference)) {
				t.Errorf("restored %s, API returned %s", got, reference)
			}
		})
	}
}

func restoreReport(path string, source reportSource, rows []any) any {
	selected := []object{}
	base, report, _ := strings.Cut(path, "/stats/")
	_, id, _ := strings.Cut(strings.TrimPrefix(base, "streams/"), "/")
	for _, row := range rows {
		if timestamp := streamMillis(row); timestamp < *source.After || timestamp > source.Before {
			continue
		}
		o := row.(object)
		switch source.Field {
		case "":
			selected = append(selected, o)
		case "artistIds":
			if slices.Contains(o["artistIds"].([]any), any(json.Number(id))) {
				selected = append(selected, o)
			}
		default:
			if o[source.Field] == json.Number(id) {
				selected = append(selected, o)
			}
		}
	}
	switch report {
	case "per-day":
		return restoreDaily(selected)
	case "dates":
		return restoreCalendar(selected)
	default:
		return selected
	}
}

func restoreDaily(rows []object) object {
	days := map[time.Time]playTotals{}
	var first, last time.Time
	for _, row := range rows {
		day := time.UnixMilli(streamMillis(row)).UTC().Truncate(24 * time.Hour)
		if first.IsZero() || day.Before(first) {
			first = day
		}
		if last.IsZero() || day.After(last) {
			last = day
		}
		totals := days[day]
		played, _ := row["playedMs"].(json.Number).Int64()
		days[day] = playTotals{totals.Count + 1, totals.DurationMs + played}
	}
	daily := object{}
	average := object{"count": nil, "durationMs": nil}
	if len(days) > 0 {
		var cum playTotals
		for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
			total := days[day]
			cum.Count += total.Count
			cum.DurationMs += total.DurationMs
			daily[day.Format("2006-01-02T00:00:00.000Z")] = object{
				"count": total.Count, "durationMs": total.DurationMs, "cumulative": cum,
			}
		}
		average = object{"count": float64(cum.Count) / float64(len(daily)), "durationMs": float64(cum.DurationMs) / float64(len(days))}
	}
	return object{"average": average, "days": daily}
}

func restoreCalendar(rows []object) object {
	years, months := map[string]playTotals{}, map[string]playTotals{}
	hours, monthDays, weekdays := map[string]playTotals{}, map[string]playTotals{}, map[string]playTotals{}
	add := func(buckets map[string]playTotals, key int, played int64) {
		k := strconv.Itoa(key)
		total := buckets[k]
		buckets[k] = playTotals{total.Count + 1, total.DurationMs + played}
	}
	for _, row := range rows {
		stamp := time.UnixMilli(streamMillis(row)).UTC()
		played, _ := row["playedMs"].(json.Number).Int64()
		add(years, stamp.Year(), played)
		add(months, int(stamp.Month()), played)
		add(hours, stamp.Hour(), played)
		add(monthDays, stamp.Day(), played)
		add(weekdays, (int(stamp.Weekday())+6)%7+1, played)
	}
	return object{"years": years, "months": months, "hours": hours, "monthDays": monthDays,
		"days": monthDays, "weekDays": weekdays, "weekdays": weekdays}
}

func TestDailyGapAndSunday(t *testing.T) {
	rows := decodeResponse([]byte(`[
		{"endTime":"2025-01-05T00:00:00Z","playedMs":80},
		{"endTime":"2025-01-03T23:59:59.999Z","playedMs":40}
	]`)).([]any)
	plays := []object{rows[0].(object), rows[1].(object)}
	got, err := json.Marshal(restoreDaily(plays))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
		"average":{"count":0.6666666666666666,"durationMs":60},
		"days":{
			"2025-01-03T00:00:00.000Z":{"count":1,"durationMs":40,"cumulative":{"count":1,"durationMs":40}},
			"2025-01-04T00:00:00.000Z":{"count":0,"durationMs":0,"cumulative":{"count":1,"durationMs":40}},
			"2025-01-05T00:00:00.000Z":{"count":1,"durationMs":80,"cumulative":{"count":2,"durationMs":120}}
		}
	}`
	if !reflect.DeepEqual(decodeResponse(got), decodeResponse([]byte(want))) {
		t.Errorf("daily gap: %s", got)
	}
	calendar := restoreCalendar(plays)
	if got := calendar["weekDays"].(map[string]playTotals)["7"]; got != (playTotals{1, 80}) {
		t.Errorf("ISO Sunday: %+v", got)
	}
}
