# stats-fm-parser

Export public stats.fm listening data and Spotify Liked Songs to JSON and JSONL. Requires Linux and Go 1.25+.

## Run

```bash
go run .
```

Enter a stats.fm username, choose a period with ← and →, then press Enter. Press Esc to stop and save a partial export. On Omarchy, the terminal UI uses the active theme.

Press F2 to switch to Spotify. Enter the path to a Spotify Account data ZIP or its extracted `YourLibrary.json` (up to 64 MiB). The export reads your library snapshot locally. [Request your Spotify data](https://www.spotify.com/account/privacy/).

## Command line

```bash
go run . -user USERNAME -period week
go run . -user USERNAME -period custom -from 2026-01-01 -to 2026-01-31
go run . -resume "exports/SESSION"
go run . -spotify-library "Spotify Account Data.zip"
```

Periods are `week`, `weeks` (4 weeks), `months` (6 months), `ytd` (since January 1), and `custom` (inclusive UTC dates). Omit `-period` for all standard stats.fm periods, including available recaps and platform rankings. Use `-out DIR` to change the default `exports/` directory. In batch mode, Ctrl+C stops a stats.fm export and saves its progress.

## Output

| File | Contents |
| --- | --- |
| `summary.json` | stats.fm totals and the first 20 entries in each main ranking |
| `streams.jsonl` | Complete event records from the general listening history |
| `rankings.jsonl`, `statistics.jsonl`, `recaps.jsonl` | Reports returned by stats.fm. Statistics also includes public profile data, current and recent listening, and any individual history fetched as a fallback |
| `metadata.jsonl` | Catalog responses for tracks, albums, and artists with missing metadata |
| `pages.jsonl` | Journal with the original response envelopes, accepted rows, and pagination checkpoints |
| `manifest.json` | Format version, result counts, and per-query coverage |
| `spotify_liked_tracks.jsonl` | Spotify track titles, artists, albums, and URIs in archive order |

Each export gets its own directory. Keep `session.json` and `pages.jsonl` to resume a stats.fm export. New sessions use format version 2. A version 1 session resumes with its original queries and file layout. When reading version 1 history, select rows with `method == "streams"` for one record per play.

Version 2 fetches the general history once and can contain fewer ready-made reports. A `reconstructible` entry in `manifest.json` gives the source file and query, the inclusive UTC bounds in Unix milliseconds, and, for an entity, the field that identifies it. A missing lower bound denotes all available history up to the given upper bound. The other statuses distinguish empty, private, unavailable, and incomplete results. The exporter fetches the original report when history coverage or required fields are missing.

| Report | Reconstruction rule and required data |
| --- | --- |
| Track, album, or artist history | Filter the complete history by `trackId`, `albumId`, or membership in `artistIds`. Keep the original event fields and order, including optional release IDs. The cursor uses `endTime` and event `id` to preserve distinct plays at the same millisecond. |
| UTC `stats/per-day` for a fixed window | Use `endTime` and `playedMs` after any entity filter. Sum counts and durations by UTC day, fill the days between the first and last play with zeros where needed, and calculate cumulative totals. Average counts over that span and durations over days with plays. Empty histories have an empty day map and null averages. |
| UTC `stats/dates` for a fixed window | With the same inputs, group counts and durations by year, month number, day of month, ISO weekday, and hour. The API also returns the day-of-month and weekday buckets under the aliases `days` and `weekdays`. |
| COUNT, TIME, and platform rankings | Stored from the API. COUNT and TIME include server ordering and position changes relative to the comparison period. |
| Detailed `stats` totals | Stored from the API. The track batch stats endpoint returns count and duration while individual stats also include percentiles and distinct-entity counts. |

Full exports also request public profiles, visibility settings, friends, achievements, recaps, records, and listener places. Access errors are recorded in the manifest. Catalog metadata is reused from complete ranking records, then fetched by unique ID in batches. Only IDs missing from the batch response are fetched individually. [stats.fm API](https://api.stats.fm/swagger) · [Stream schema](https://github.com/statsfm/statsfm.js/blob/main/src/interfaces/statsfm/stream.ts) · [Ranking methods](https://support-new.stats.fm/hc/en-us/articles/61358470671513-Differences-between-calculation-methods)

## Development

- `internal/app` handles CLI flags, the terminal UI, and export selection.
- `internal/statsfm` handles API requests, export sessions, and resume.
- `internal/spotify` imports Liked Songs from account archives.
- `internal/exportfs` provides atomic file writes shared by both exporters.

Run checks with `go test -race ./...` and `go vet ./...`. The stats.fm tests include anonymized API responses for the same closed period, validate the recovery rules, and compare version 1 and 2 request counts on identical input.
