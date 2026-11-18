package statsfm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func readOutput(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestExportResume(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		key := r.URL.String()
		mu.Lock()
		hits[key]++
		attempt := hits[key]
		mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/id/")
		switch {
		case r.URL.Path == "/name":
			fmt.Fprint(w, `{"item":{"id":"id","customId":"name","spotifyAuth":{}}}`)
		case path == "top/tracks" && q.Get("orderBy") == "COUNT" && q.Get("range") == "today":
			switch q.Get("offset") {
			case "0":
				if attempt == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(429)
					return
				}
				fmt.Fprint(w, `{"items":[{"position":1,"streams":0,"track":{"id":9007199254740993,"name":"First","artists":[{"id":1,"name":"A"}]}}]}`)
			default:
				fmt.Fprint(w, `{"items":[{"position":2,"track":{"id":2,"name":"Second","artists":[{"id":1,"name":"A"}]}}]}`)
			}
		case path == "top/artists/1/albums" && q.Get("offset") == "0" && q.Get("range") == "today" && q.Get("orderBy") == "COUNT":
			fmt.Fprint(w, `{"items":[{"position":1,"album":{"id":3}}]}`)
		case path == "friends/count":
			fmt.Fprint(w, `{"item":4,"source":"api"}`)
		case r.URL.Path == "/tracks/9007199254740993/top/listeners/place/id":
			fmt.Fprint(w, `{"item":{"place":3,"unknownPlaceField":true}}`)
		case path == "streams/current":
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"hidden"}`)
		case path == "recaps/yearly":
			fmt.Fprint(w, "unexpected plain text")
		case path == "recaps/monthly":
			fmt.Fprint(w, `{"id":"monthly","topTracks":[]}`)
		case path == "streams/stats":
			fmt.Fprint(w, `{"items":{"count":0}}`)
		default:
			fmt.Fprint(w, `{"items":[]}`)
		}
	}))
	defer server.Close()
	c := &client{base: server.URL + "/", http: server.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	topPages := 0
	dir, err := c.export(ctx, Options{User: "name", Out: t.TempDir(), From: "2025-01-01", To: "2025-01-01"}, func(u Update) {
		if u.Progress.Path == "top/tracks" && u.Progress.Period == "today" && u.Progress.Order == "COUNT" {
			topPages++
			if topPages == 2 {
				cancel()
			}
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if !strings.Contains(string(readOutput(t, dir, "manifest.json")), `"state":"interrupted"`) {
		t.Fatal("missing interruption")
	}
	// Simulate a process killed in the middle of the next journal entry.
	f, err := os.OpenFile(filepath.Join(dir, "pages.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"query":`); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = c.export(context.Background(), Options{Resume: dir}, func(Update) {}); err != nil {
		t.Fatal(err)
	}
	var man struct {
		State   string
		Queries []result
	}
	if err = json.Unmarshal(readOutput(t, dir, "manifest.json"), &man); err != nil {
		t.Fatal(err)
	}
	if man.State != "finished_with_gaps" {
		t.Fatal(man.State)
	}
	byQuery := map[query]result{}
	for _, r := range man.Queries {
		byQuery[r.Query] = r
	}
	top := query{Path: "top/tracks", Period: "today", Order: "COUNT"}
	if r := byQuery[top]; r.Count != 2 || r.Status != "partial" {
		t.Fatal(r)
	}
	if byQuery[query{Path: "streams/current"}].Status != "private" || byQuery[query{Path: "recaps/yearly"}].Status != "partial" || byQuery[query{Path: "recaps/monthly"}].Count != 1 {
		t.Fatal("incorrect coverage")
	}
	if byQuery[query{Path: "top/albums/3/tracks", Period: "today", Order: "COUNT"}].Status != "empty" {
		t.Fatal("nested album was skipped")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/name"] != 1 || hits["/id/top/tracks?limit=100&offset=0&orderBy=COUNT&range=today"] != 2 {
		t.Fatal("completed request was repeated after resume")
	}
	for _, path := range []string{"/id/profile", "/id/privacy", "/id/friends", "/id/friends/count", "/id/achievements/completed",
		"/id/achievements/uncompleted", "/id/streams/recent", "/id/recaps/time-capsule", "/id/records/artists", "/tracks/9007199254740993/top/listeners/place/id"} {
		if hits[path] != 1 {
			t.Errorf("public query %s: %d requests", path, hits[path])
		}
	}
	stats := string(readOutput(t, dir, "statistics.jsonl"))
	if !strings.Contains(stats, `"data":4`) || !strings.Contains(stats, `"unknownPlaceField":true`) {
		t.Fatal("public data missing from statistics.jsonl")
	}

	var summary struct{ Rankings []summaryGroup }
	if err = json.Unmarshal(readOutput(t, dir, "summary.json"), &summary); err != nil {
		t.Fatal(err)
	}
	n := slices.IndexFunc(summary.Rankings, func(g summaryGroup) bool { return g.Query == top })
	if n < 0 {
		t.Fatal("missing top")
	}
	rows := summary.Rankings[n].Rows
	if len(rows) != 2 || rows[0]["streams"] != float64(0) || rows[1]["streams"] != nil || rows[0]["name"] != "First" {
		t.Fatal(rows)
	}
	b := string(readOutput(t, dir, "rankings.jsonl"))
	if !strings.Contains(b, "9007199254740993") || strings.Contains(b, `"playedMs":null`) {
		t.Fatal(b)
	}
	for _, name := range []string{"rankings", "streams", "statistics", "recaps"} {
		for _, line := range strings.Split(strings.TrimSpace(string(readOutput(t, dir, name+".jsonl"))), "\n") {
			if line != "" && !json.Valid([]byte(line)) {
				t.Fatal(line)
			}
		}
	}
}
func TestInclusiveHistoryCursor(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var cursors []int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("offset") {
			t.Error("offset pagination on history")
		}
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		cursors = append(cursors, before)
		var rows []object
		for n, ms := range []int64{3, 2, 2, 1} {
			if ms <= before-epoch.UnixMilli() {
				rows = append(rows, object{"id": fmt.Sprint(n), "endTime": epoch.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano)})
				if len(rows) == 3 {
					break
				}
			}
		}
		json.NewEncoder(w).Encode(object{"items": rows})
	}))
	defer server.Close()
	c := client{base: server.URL + "/", http: server.Client()}
	q := query{Path: "streams"}
	cfg := config{Profile: profile{ID: "id"}, Started: epoch.Add(4 * time.Millisecond)}
	idx := newIndex()
	r := &result{}
	for n := 0; n < 8; n++ {
		p, err := c.fetch(context.Background(), cfg, q, r)
		if err != nil {
			t.Fatal(err)
		}
		idx.apply(p)
		r = idx.results[q]
		if r.Status != "running" {
			break
		}
	}
	if r.Status != "complete" || r.Count != 4 || len(cursors) != 4 {
		t.Fatalf("%+v cursors %v", r, cursors)
	}
}
func TestSummaryLimitAndLock(t *testing.T) {
	dir := t.TempDir()
	cfg := config{Profile: profile{ID: "u"}, Started: time.Now(), Periods: []string{"today"}}
	s, err := openStore(dir, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if other, err := openStore(dir, nil); err == nil {
		other.close()
		t.Fatal("concurrent writer")
	}
	p := page{Query: query{Path: "top/artists", Period: "today", Order: "TIME"}, Status: "complete"}
	for i := 1; i <= 22; i++ {
		p.Rows = append(p.Rows, object{"position": i, "artist": object{"id": i}})
	}
	idx := newIndex()
	idx.apply(p)
	if err = s.append(p); err != nil {
		t.Fatal(err)
	}
	if err = s.publish(idx, "finished", nil); err != nil {
		t.Fatal(err)
	}
	var summary struct{ Rankings []summaryGroup }
	if err = json.Unmarshal(readOutput(t, dir, "summary.json"), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Rankings[0].Rows) != 20 || strings.Count(string(readOutput(t, dir, "rankings.jsonl")), "\n") != 22 {
		t.Fatal("summary limit leaked into export")
	}
}
func TestDateValidationAndDiscovery(t *testing.T) {
	if err := (Options{User: "u", From: "2025-02-01", To: "2025-01-01"}).Validate(); err == nil {
		t.Fatal("inverted dates")
	}
	q := query{Period: "custom"}
	v := q.params(config{From: "1970-01-01", To: "1970-01-01"})
	if v.Get("after") != "0" || v.Get("before") != "86399999" || v.Has("range") {
		t.Fatal(v)
	}
	if retryDelay("2", time.Second) != 2*time.Second || retryDelay("broken", time.Second) != time.Second {
		t.Fatal("retry delay")
	}
	a := newIndex()
	a.apply(page{Query: query{Path: "streams"}, Rows: []any{object{"trackId": json.Number("3"), "artistIds": []any{json.Number("8"), json.Number("8")}}}})
	if !reflect.DeepEqual(a.entities, []entity{{"artists", "8"}, {"tracks", "3"}}) {
		t.Fatal(a.entities)
	}
}
