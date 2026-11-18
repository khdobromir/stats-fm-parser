package statsfm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const firstPlay = `{"id":"a","userId":"id","endTime":"2025-01-02T23:59:59.999Z","playedMs":120,"trackId":1,"trackName":"A","albumId":3,"artistIds":[5,6],"trackReleaseId":17,"albumReleaseId":23,"importId":9}`
const secondPlay = `{"id":"b","userId":"id","endTime":"2025-01-02T23:59:59.999Z","playedMs":70,"trackId":2,"trackName":"B","albumId":4,"artistIds":[6],"contextId":"synthetic"}`
const thirdPlay = `{"id":"c","userId":"id","endTime":"2025-01-02T00:00:00Z","playedMs":100,"trackId":1,"trackName":"A","albumId":3,"artistIds":[5,6],"trackReleaseId":18,"albumReleaseId":24}`

func TestVersionedHistoryCoverage(t *testing.T) {
	var legacyRequests int
	for _, mode := range []string{"legacy", "complete", "missing_artist_ids", "missing_played_ms", "private"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			hits := map[string]int{}
			total := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				hits[r.URL.Path]++
				total++
				mu.Unlock()
				path := r.URL.Path
				switch path {
				case "/users/name":
					fmt.Fprint(w, `{"item":{"id":"id","customId":"name","unknownProfileField":42}}`)
				case "/users/id/streams":
					if mode == "private" {
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `{"message":"hidden","errorDetails":{"field":7}}`)
						return
					}
					switch r.URL.Query().Get("before") {
					case "1735862399999":
						b := secondPlay
						switch mode {
						case "missing_artist_ids":
							b = strings.Replace(b, `"artistIds":[6],`, "", 1)
						case "missing_played_ms":
							b = strings.Replace(b, `"playedMs":70,`, "", 1)
						}
						fmt.Fprintf(w, `{"items":[%s,%s,%s],"next":{"opaque":true}}`, firstPlay, b, thirdPlay)
					case "1735776000000":
						fmt.Fprintf(w, `{"items":[%s]}`, thirdPlay)
					default:
						t.Errorf("unexpected history cursor %s", r.URL.RawQuery)
					}
				case "/users/id/top/tracks":
					if r.URL.Query().Get("offset") == "0" {
						fmt.Fprint(w, `{"items":[{"position":1,"track":{"id":1,"name":"A","artists":[{"id":6}],"albums":[{"id":3}]},"indicator":"UP"}]}`)
					} else {
						fmt.Fprint(w, `{"items":[]}`)
					}
				case "/tracks/list":
					ids := r.URL.Query().Get("ids")
					if ids != "1,2" && ids != "1" {
						t.Errorf("track batch ids = %q", ids)
					}
					fmt.Fprint(w, `{"items":[{"id":1,"name":"A","unknownCatalogField":true}]}`)
				case "/tracks/2":
					fmt.Fprint(w, `{"item":{"id":2,"name":"B"}}`)
				case "/artists/list", "/albums/list":
					rows := []object{}
					for id := range strings.SplitSeq(r.URL.Query().Get("ids"), ",") {
						rows = append(rows, object{"id": json.Number(id), "name": "Catalog"})
					}
					json.NewEncoder(w).Encode(object{"items": rows})
				default:
					if strings.HasPrefix(path, "/users/id/streams/") && strings.Count(path, "/") == 5 &&
						!strings.Contains(path, "stats") && r.URL.Query().Get("before") == "1735862399999" {
						fmt.Fprint(w, `{"items":[{"id":"fallback","endTime":"2025-01-02T00:00:00Z","unknownHistoryField":17}]}`)
						return
					}
					fmt.Fprint(w, `{"items":[]}`)
				}
			}))
			defer server.Close()
			c := &client{base: server.URL + "/users/", http: server.Client()}
			o := Options{User: "name", Out: t.TempDir(), Period: CustomPeriod, From: "2025-01-02", To: "2025-01-02"}
			if mode == "legacy" {
				p, _, err := c.profile(context.Background(), "name")
				if err != nil {
					t.Fatal(err)
				}
				cfg := config{Profile: p, Periods: []string{CustomPeriod},
					Started: time.Now(), Window: &dateWindow{After: 1735776000000, Before: 1735862399999}}
				dir := t.TempDir()
				s, err := openStore(dir, &cfg)
				if err != nil {
					t.Fatal(err)
				}
				s.close()
				o = Options{Resume: dir}
			}
			if mode == "complete" {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				pages := 0
				dir, err := c.export(ctx, o, func(u Update) {
					if u.Progress.Path == "streams" {
						pages++
						if pages == 2 {
							cancel()
						}
					}
				})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("interrupted history: %v", err)
				}
				o = Options{Resume: dir}
			}
			dir, err := c.export(context.Background(), o, func(Update) {})
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Version int
				Queries []result
			}
			if err := json.Unmarshal(readOutput(t, dir, "manifest.json"), &manifest); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			beforeResume := total
			if mode == "legacy" {
				legacyRequests = total
				valid := manifest.Version == 1 && hits["/users/id/streams/tracks/1"] > 0 && hits["/tracks/list"] == 0
				mu.Unlock()
				if !valid {
					t.Fatalf("legacy version %d, hits %v", manifest.Version, hits)
				}
				return
			}
			if manifest.Version != 2 || hits["/users/name"] != 1 || hits["/tracks/list"] != 1 || hits["/tracks/1"] != 0 {
				t.Errorf("version %d, hits %v", manifest.Version, hits)
			}
			if mode == "complete" {
				t.Logf("requests on the same input: v1=%d v2=%d", legacyRequests, total)
				if total >= legacyRequests || hits["/users/id/streams/stats"] != 1 || hits["/users/id/top/tracks"] == 0 {
					t.Errorf("missing savings, ranking or detailed statistics: %v", hits)
				}
			}
			for _, path := range []string{"streams/stats/dates", "streams/stats/per-day", "streams/artists/6/stats/dates", "streams/tracks/1/stats/per-day"} {
				fallback := mode == "private" || mode == "missing_played_ms" ||
					mode == "missing_artist_ids" && strings.Contains(path, "artists/6")
				if requested := hits["/users/id/"+path] > 0; requested != fallback {
					t.Errorf("%s requested = %v, fallback = %v", path, requested, fallback)
				}
			}
			if mode != "private" && (hits["/users/id/streams"] != 2 || hits["/tracks/2"] != 1 ||
				bytes.Count(readOutput(t, dir, "streams.jsonl"), []byte("\n")) != 3) {
				t.Errorf("missing rows, incorrect pagination or metadata fallback: %v", hits)
			}
			for _, kind := range []string{"tracks/1", "albums/3", "artists/6"} {
				path := "streams/" + kind
				fallback := mode == "private" || mode == "missing_artist_ids" && kind == "artists/6"
				if requested := hits["/users/id/"+path] > 0; requested != fallback {
					t.Errorf("%s requested = %v, fallback = %v", path, requested, fallback)
				}
				if !fallback {
					found := false
					for _, q := range manifest.Queries {
						if q.Query.Path == path {
							found = q.Status == "reconstructible" && q.Source != nil && q.Source.File == "streams.jsonl" &&
								q.Source.After != nil && *q.Source.After == 1735776000000 && q.Source.Before == 1735862399999
						}
					}
					if !found {
						t.Errorf("missing manifest coverage for %s", path)
					}
				}
			}
			mu.Unlock()
			journal := readOutput(t, dir, "pages.jsonl")
			if !bytes.Contains(journal, []byte(`"unknownProfileField":42`)) ||
				(mode != "private" && !bytes.Contains(journal, []byte(`"next":{"opaque":true}`))) {
				t.Fatal("missing original API response")
			}
			if (mode == "missing_artist_ids" || mode == "private") && !bytes.Contains(readOutput(t, dir, "statistics.jsonl"), []byte(`"unknownHistoryField":17`)) {
				t.Fatal("missing fallback history")
			}
			if _, err := c.export(context.Background(), Options{Resume: dir}, func(Update) {}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if total != beforeResume {
				t.Fatalf("resume repeated requests: %v", hits)
			}
		})
	}
}

func TestHistoryCursorAtSessionBoundary(t *testing.T) {
	for _, count := range []int{1, pageSize} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			started := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
			var mu sync.Mutex
			var requested []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				before := r.URL.Query().Get("before")
				mu.Lock()
				requested = append(requested, before)
				mu.Unlock()
				var rows []object
				if before == "1735776000000" {
					for n := range count {
						rows = append(rows, object{"id": fmt.Sprint(n), "endTime": "2025-01-02T00:00:00Z"})
					}
				}
				json.NewEncoder(w).Encode(object{"items": rows})
			}))
			defer server.Close()
			c := &client{base: server.URL + "/users/", http: server.Client()}
			cfg := config{Version: 2, Profile: profile{ID: "id"}, Started: started}
			idx := newIndex()
			q := query{Path: "streams"}
			r := &result{}
			for range 4 {
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
			wantStatus, wantCursors := "complete", "1735776000000,1735775999999"
			if count == pageSize {
				wantStatus, wantCursors = "partial", "1735776000000"
			}
			mu.Lock()
			defer mu.Unlock()
			if r.Status != wantStatus || r.Count != count || strings.Join(requested, ",") != wantCursors {
				t.Fatalf("history: %d %s, cursors %v", r.Count, r.Status, requested)
			}
		})
	}
}

func TestCatalogBatchesUseCompleteRankingMetadata(t *testing.T) {
	raw := json.RawMessage(`{"items":[
		{"artist":{"id":1,"externalIds":{},"followers":0,"genres":[],"image":null,"name":"A","spotifyPopularity":0}},
		{"artist":{"id":2,"name":"B"}}
	]}`)
	q := query{Path: "top/artists", Order: "COUNT"}
	rows, err := rowsFor(q, decodeResponse(raw), config{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	idx := newIndex()
	idx.apply(page{Query: q, Rows: rows, Response: raw, Status: "complete"})
	batches := idx.metadataBatches()
	if len(batches) != 1 || batches[0].Path != "artists/list" || batches[0].IDs != "2" {
		t.Fatalf("catalog batches: %+v", batches)
	}
}
