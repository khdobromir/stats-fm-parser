package statsfm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestVersionedPageLimits(t *testing.T) {
	limits := make(chan string, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limits <- r.URL.Query().Get("limit")
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()
	c := &client{base: server.URL + "/", http: server.Client()}
	for _, tc := range []struct {
		version     int
		path, limit string
	}{
		{1, "streams", "100"}, {2, "top/tracks", "100"},
		{3, "streams", "1000"}, {3, "top/tracks", "500"}, {3, "streams/artists/1", "100"},
	} {
		cfg := config{Version: tc.version, Profile: profile{ID: "id"}, Started: time.Now()}
		if _, err := c.fetch(context.Background(), cfg, query{Path: tc.path}, &result{}); err != nil {
			t.Fatal(err)
		}
		if got := <-limits; got != tc.limit {
			t.Errorf("v%d %s limit %s, want %s", tc.version, tc.path, got, tc.limit)
		}
	}
}

func TestHistoryArchiveOnlyRequestsHistoryAndCatalog(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/users/name":
			fmt.Fprint(w, `{"item":{"id":"id","customId":"name"}}`)
		case "/users/id/streams":
			if !r.URL.Query().Has("after") {
				t.Error("missing closed window")
			}
			if r.URL.Query().Get("before") >= "1735776000000" {
				fmt.Fprint(w, `{"items":[`+thirdPlay+`]}`)
			} else {
				fmt.Fprint(w, `{"items":[]}`)
			}
		case "/tracks/list":
			fmt.Fprint(w, `{"items":[{"id":1,"name":"Track"}]}`)
		case "/albums/list":
			fmt.Fprint(w, `{"items":[{"id":3,"name":"Album"}]}`)
		case "/artists/list":
			fmt.Fprint(w, `{"items":[{"id":5,"name":"Artist"},{"id":6,"name":"Artist"}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			fmt.Fprint(w, `{"items":[]}`)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL + "/users/", http: server.Client()}
	cfg := Options{User: "name", Out: t.TempDir(), Period: CustomPeriod, From: "2025-01-02", To: "2025-01-02"}
	cfg.Mode = HistoryMode
	dir, err := c.export(context.Background(), cfg, func(Update) {})
	if err != nil {
		t.Fatal(err)
	}
	_ = readOutput(t, dir, "streams.jsonl")
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"/albums/list", "/artists/list", "/tracks/list", "/users/id/streams", "/users/id/streams", "/users/name"}) {
		t.Fatalf("paths: %v", paths)
	}
}

func TestUpdateKeepsSnapshotUntilHistorySucceeds(t *testing.T) {
	var fail atomic.Bool
	var updated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/name":
			fmt.Fprint(w, `{"item":{"id":"id","customId":"name"}}`)
		case "/users/id/streams":
			if fail.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if r.URL.Query().Get("before") < "1735776000000" {
				fmt.Fprint(w, `{"items":[]}`)
				return
			}
			row := thirdPlay
			if updated.Load() {
				row = strings.Replace(row, `"playedMs":100`, `"playedMs":200`, 1)
			}
			fmt.Fprint(w, `{"items":[`+row+`]}`)
		default:
			fmt.Fprint(w, `{"items":[]}`)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL + "/users/", http: server.Client()}
	root, err := c.export(context.Background(), Options{User: "name", Out: t.TempDir(), Mode: HistoryMode,
		Period: CustomPeriod, From: "2025-01-02", To: "2025-01-02"}, func(Update) {})
	if err != nil {
		t.Fatal(err)
	}
	old := readOutput(t, root, "streams.jsonl")
	fail.Store(true)
	update := Options{Update: root}
	if _, err := c.export(context.Background(), update, func(Update) {}); err == nil {
		t.Fatal("expected update error")
	}
	if got := readOutput(t, root, "streams.jsonl"); !bytes.Equal(old, got) {
		t.Fatal("failed update replaced snapshot")
	}
	fail.Store(false)
	updated.Store(true)
	if _, err := c.export(context.Background(), Options{Resume: root}, func(Update) {}); err != nil {
		t.Fatal(err)
	}
	got := readOutput(t, root, "streams.jsonl")
	if !bytes.Contains(got, []byte(`"playedMs":200`)) || bytes.Count(got, []byte("\n")) != 1 {
		t.Fatalf("updated stream: %s", got)
	}
}

func TestImportCompleteLegacyHistory(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			source := t.TempDir()
			cfg := config{Version: version, Profile: profile{ID: "id"}, Started: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC), Periods: []string{"lifetime"}}
			s, err := openStore(source, &cfg)
			if err != nil {
				t.Fatal(err)
			}
			play := decodeResponse([]byte(thirdPlay))
			for _, p := range []page{
				{Query: query{Path: "streams"}, Status: "complete", Rows: []any{play}},
				{Query: query{Path: "streams/tracks/1"}, Status: "complete", Rows: []any{play}},
				{Query: query{Path: "tracks/list", IDs: "1"}, Status: "complete", Rows: []any{object{"id": 1, "name": "Track"}}},
			} {
				if err := s.append(p); err != nil {
					t.Fatal(err)
				}
			}
			s.close()
			before := readOutput(t, source, "pages.jsonl")
			dir, err := (&client{}).export(context.Background(), Options{ImportHistory: source, Out: t.TempDir()}, func(Update) {})
			if err != nil {
				t.Fatal(err)
			}
			if rows := readOutput(t, dir, "streams.jsonl"); bytes.Count(rows, []byte("\n")) != 1 {
				t.Fatalf("imported rows: %s", rows)
			}
			if rows := readOutput(t, dir, "metadata.jsonl"); !bytes.Contains(rows, []byte(`"name":"Track"`)) {
				t.Fatalf("missing imported metadata: %s", rows)
			}
			if !bytes.Equal(before, readOutput(t, source, "pages.jsonl")) {
				t.Fatal("import modified its source")
			}
		})
	}
}

func TestArchiveRefreshWindow(t *testing.T) {
	date := func(s string) time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, tc := range []struct{ period, refresh, after string }{
		{"", "", "2026-11-03"},
		{"", "2025-01-01", "2025-01-01"},
		{"week", "2025-01-01", "2026-11-04"},
		{"custom", "2025-01-01", "2026-11-01"},
	} {
		opts := Options{Period: tc.period, Mode: HistoryMode}
		if tc.period == CustomPeriod {
			opts.From, opts.To = "2026-11-01", "2026-11-10"
		}
		p := opts.exportConfig(profile{ID: "id"}, date("2026-11-10"))
		cfg, err := updateConfig(p, ".snapshot-base", tc.refresh, date("2026-11-11"))
		if err != nil || cfg.Scan.After != date(tc.after).UnixMilli() {
			t.Errorf("%+v: scan %+v, err %v", tc, cfg.Scan, err)
		}
	}
}

func TestSessionPerformanceOnSameInput(t *testing.T) {
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := active.Add(1); n > peak.Load() {
			for previous := peak.Load(); n > previous && !peak.CompareAndSwap(previous, n); previous = peak.Load() {
			}
		}
		defer active.Add(-1)
		time.Sleep(4 * time.Millisecond)
		switch r.URL.Path {
		case "/users/name", "/users/id":
			fmt.Fprint(w, `{"item":{"id":"id","customId":"name"}}`)
		case "/users/id/streams":
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
			start := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC).UnixMilli()
			rows := []object{}
			for n := 299; n >= 0 && len(rows) < limit; n-- {
				ms := start + int64(n/2)
				if ms <= before {
					rows = append(rows, object{"id": fmt.Sprint(n), "endTime": time.UnixMilli(ms).UTC().Format(time.RFC3339Nano), "playedMs": 123, "trackId": 1, "albumId": 2, "artistIds": []int{3}})
				}
			}
			json.NewEncoder(w).Encode(object{"items": rows})
		case "/users/id/top/tracks":
			if r.URL.Query().Get("offset") == "0" {
				fmt.Fprint(w, `{"items":[{"position":1,"track":{"id":1},"streams":300}]}`)
			} else {
				fmt.Fprint(w, `{"items":[]}`)
			}
		case "/users/id/streams/stats":
			fmt.Fprint(w, `{"item":{"count":300}}`)
		case "/tracks/list":
			fmt.Fprint(w, `{"items":[{"id":1,"name":"Track"}]}`)
		case "/albums/list":
			fmt.Fprint(w, `{"items":[{"id":2,"name":"Album"}]}`)
		case "/artists/list":
			fmt.Fprint(w, `{"items":[{"id":3,"name":"Artist"}]}`)
		default:
			fmt.Fprint(w, `{"items":[]}`)
		}
	}))
	defer server.Close()
	var baselineRows []string
	var baselineCoverage []result
	var baselineReports map[query][]string
	var fullRequests int
	for _, run := range []struct {
		mode             string
		version, workers int
	}{
		{"full", 2, 1}, {"full", 3, 1}, {"full", 3, 3}, {HistoryMode, 3, 3},
	} {
		peak.Store(0)
		c := &client{base: server.URL + "/users/", http: server.Client(), workers: run.workers}
		o := Options{User: "name", Out: t.TempDir(), Period: CustomPeriod, From: "2025-01-02", To: "2025-01-02", Mode: run.mode}
		if run.version == 2 {
			cfg := o.exportConfig(profile{ID: "id", Orders: []string{"COUNT", "TIME", "default"}}, time.Now())
			cfg.Version, cfg.Mode = 2, ""
			s, err := openStore(o.Out, &cfg)
			if err != nil {
				t.Fatal(err)
			}
			s.close()
			o = Options{Resume: o.Out}
		}
		start := time.Now()
		dir, err := c.export(context.Background(), o, func(Update) {})
		if err != nil {
			t.Fatal(err)
		}
		var rows []string
		dec := json.NewDecoder(bytes.NewReader(readOutput(t, dir, "streams.jsonl")))
		for dec.More() {
			var v exportRow
			if err := dec.Decode(&v); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(v.Data)
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, string(b))
		}
		if len(rows) != 300 {
			t.Fatalf("got %d events", len(rows))
		}
		if baselineRows == nil {
			baselineRows = rows
		}
		if !slices.Equal(baselineRows, rows) {
			t.Fatal("history changed with page size or workers")
		}
		var manifest struct{ Queries []result }
		if err := json.Unmarshal(readOutput(t, dir, "manifest.json"), &manifest); err != nil {
			t.Fatal(err)
		}
		t.Logf("v%d %s workers=%d requests=%d 429=%d elapsed=%s peak=%d reports=%d", run.version, run.mode, run.workers, c.requests, c.limited, time.Since(start).Round(time.Millisecond), peak.Load(), len(manifest.Queries))
		if run.mode == "full" && run.version == 3 {
			reports := map[query][]string{}
			for _, file := range []string{"rankings.jsonl", "statistics.jsonl", "metadata.jsonl"} {
				dec := json.NewDecoder(bytes.NewReader(readOutput(t, dir, file)))
				for dec.More() {
					var row exportRow
					if err := dec.Decode(&row); err != nil {
						t.Fatal(err)
					}
					b, err := json.Marshal(row.Data)
					if err != nil {
						t.Fatal(err)
					}
					reports[row.query] = append(reports[row.query], string(b))
				}
			}
			if baselineCoverage == nil {
				baselineCoverage = manifest.Queries
				baselineReports = reports
				fullRequests = c.requests
			} else if !reflect.DeepEqual(baselineCoverage, manifest.Queries) || !reflect.DeepEqual(baselineReports, reports) || fullRequests != c.requests {
				t.Fatal("parallel full export changed coverage")
			}
		}
		if run.workers == 3 && peak.Load() != 3 {
			t.Fatalf("peak concurrency %d", peak.Load())
		}
		if run.mode == HistoryMode && c.requests >= fullRequests {
			t.Fatal("history did not reduce requests")
		}
	}
}

func TestSharedRetryAfter(t *testing.T) {
	started := make(chan time.Time, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/limited" {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		started <- time.Now()
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()
	c := &client{http: server.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _, _ = c.get(ctx, server.URL+"/limited", nil) }()
	deadline := time.Now().Add(time.Second)
	var cooldown time.Time
	for cooldown.IsZero() && time.Now().Before(deadline) {
		c.mu.Lock()
		cooldown = c.cooldown
		c.mu.Unlock()
		if cooldown.IsZero() {
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
	if cooldown.IsZero() {
		t.Fatal("429 failed to pause other requests")
	}
	if _, err := c.get(context.Background(), server.URL+"/other", nil); err != nil {
		t.Fatal(err)
	}
	if when := <-started; when.Before(cooldown) {
		t.Fatalf("started before Retry-After: %v < %v", when, cooldown)
	}
}

func TestHistoryDatesSelectBoundedScan(t *testing.T) {
	o := Options{Mode: HistoryMode, From: "2025-01-02", To: "2025-01-02"}
	cfg := o.exportConfig(profile{}, time.Now())
	if cfg.historyQuery().Period != CustomPeriod || cfg.Window == nil || cfg.Window.After != 1735776000000 {
		t.Fatalf("unexpected history window %+v", cfg)
	}
}

func TestUpdateAddsLateImportWithoutDuplicates(t *testing.T) {
	source := t.TempDir()
	s, err := openStore(source, &config{Version: 2, Profile: profile{ID: "id"}, Started: time.Date(2025, 1, 25, 0, 0, 0, 0, time.UTC), Periods: []string{"lifetime"}})
	if err != nil {
		t.Fatal(err)
	}
	saved := strings.ReplaceAll(strings.ReplaceAll(thirdPlay, `"id":"c"`, `"id":"saved"`), "2025-01-02", "2025-01-24")
	if err := s.append(page{Query: query{Path: "streams"}, Status: "complete", Rows: []any{decodeResponse([]byte(saved))}}); err != nil {
		t.Fatal(err)
	}
	s.close()
	root, err := (&client{}).export(context.Background(), Options{ImportHistory: source, Out: t.TempDir()}, func(Update) {})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path == "/users/id/streams" && q.Get("after") == "1735689600000" && q.Get("before") >= "1735776000000" {
			fmt.Fprint(w, `{"items":[`+thirdPlay+`]}`)
		} else {
			fmt.Fprint(w, `{"items":[]}`)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL + "/users/", http: server.Client()}
	for _, refresh := range []string{"2025-01-01", ""} {
		if _, err := c.export(context.Background(), Options{Update: root, RefreshFrom: refresh}, func(Update) {}); err != nil {
			t.Fatal(err)
		}
		rows := readOutput(t, root, "streams.jsonl")
		if bytes.Count(rows, []byte("\n")) != 2 || !bytes.Contains(rows, []byte(`"id":"c"`)) {
			t.Fatalf("late import: %s", rows)
		}
	}
}
