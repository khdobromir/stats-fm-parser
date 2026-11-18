package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestPeriodBounds(t *testing.T) {
	started := time.Date(2026, time.August, 31, 8, 30, 0, 0, time.UTC)
	for _, tc := range []struct{ period, first string }{
		{"week", "2026-08-24T08:30:00Z"},
		{"weeks", "2026-08-03T08:30:00Z"},
		{"months", "2026-02-28T08:30:00Z"},
		{"ytd", "2026-01-01T00:00:00Z"},
	} {
		t.Run(tc.period, func(t *testing.T) {
			cfg := (options{period: tc.period}).exportConfig(profile{Orders: []string{"COUNT", "TIME", "SPOTIFY", "default"}}, started)
			first, err := time.Parse(time.RFC3339, tc.first)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Window.After != first.UnixMilli() || cfg.Window.Before != started.UnixMilli() {
				t.Fatal(cfg.Window)
			}
			for _, e := range []entity{{}, {"artists", "10"}} {
				for _, q := range queries(cfg, e) {
					if q.Period != tc.period || q.Order == "SPOTIFY" || q.Order == "default" {
						t.Fatal(q)
					}
					params := q.params(cfg)
					if params.Has("range") || params.Get("after") != strconv.FormatInt(first.UnixMilli(), 10) || params.Get("before") != strconv.FormatInt(started.UnixMilli(), 10) {
						t.Fatal(params)
					}
				}
			}
		})
	}
}

func TestBoundedExportAndResume(t *testing.T) {
	started := time.Date(2026, time.November, 8, 0, 0, 0, 0, time.UTC)
	cfg := (options{period: customPeriod, from: "2026-11-05", to: "2026-11-06"}).exportConfig(profile{ID: "id", Orders: []string{"COUNT"}}, started)
	after, before := cfg.Window.After, cfg.Window.Before
	dir := t.TempDir()
	s, err := openStore(dir, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	// Run the resume entry point against a server that ignores the lower bound.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("after") != strconv.FormatInt(after, 10) || q.Get("before") != strconv.FormatInt(before, 10) {
			t.Errorf("incorrect resume parameters for %s: %v", r.URL.Path, q)
		}
		if r.URL.Path == "/id/streams" {
			rows := []object{}
			for i, ms := range []int64{before, after, after - 1} {
				rows = append(rows, object{"id": fmt.Sprint(i), "endTime": time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)})
			}
			json.NewEncoder(w).Encode(object{"items": rows})
			return
		}
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()
	c := &client{base: server.URL + "/", http: server.Client()}
	if _, err = export(context.Background(), c, options{resume: dir}, func(update) {}); err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Window dateWindow
		State  string
	}
	if err = json.Unmarshal(readOutput(t, dir, "summary.json"), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Window != *cfg.Window || summary.State != "finished" {
		t.Fatal(summary)
	}
	var manifest struct{ Rows map[string]int }
	if err = json.Unmarshal(readOutput(t, dir, "manifest.json"), &manifest); err != nil || manifest.Rows["streams"] != 2 {
		t.Fatal(manifest, err)
	}
}
