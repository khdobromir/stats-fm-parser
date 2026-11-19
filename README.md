# stats-fm-parser

Export public stats.fm listening data and Spotify Liked Songs to JSON and JSONL. Requires Linux and Go 1.25+.

## Run

```bash
go run .
```

Enter a stats.fm username, choose a period with ← and →, then press Enter. The terminal UI defaults to **History and metadata**. Press F4 to switch to **Full export**, F3 to update a history archive, or Esc during a run to pause it. On Omarchy, the terminal UI uses the active theme.

Press F2 to switch to Spotify. Enter the path to a Spotify Account data ZIP or its extracted `YourLibrary.json` (up to 64 MiB). The export reads your library snapshot locally. [Request your Spotify data](https://www.spotify.com/account/privacy/).

## Command line

```bash
go run . -user USERNAME -mode history -period week
go run . -user USERNAME -mode full -period week
go run . -user USERNAME -period custom -from 2026-01-01 -to 2026-01-31
go run . -resume "exports/SESSION"
go run . -import-history "exports/OLD-SESSION"
go run . -update "exports/HISTORY-ARCHIVE"
go run . -update "exports/HISTORY-ARCHIVE" -refresh-from 2026-01-01
go run . -spotify-library "Spotify Account Data.zip"
```

The CLI defaults to `-mode full` with `-user`. History mode requests the profile, general listening history, and catalog metadata.

Periods are `week`, `weeks` (4 weeks), `months` (6 months), `ytd` (since January 1), and `custom` (inclusive UTC dates). Omit `-period` to fetch all available history in history mode or all standard reports in full mode. Use `-out DIR` to change the default `exports/` directory. In batch mode, Ctrl+C stops a stats.fm export and saves its progress.

## Updating a history archive

Use `-update DIR` or F3 on the root directory of a history archive. Each update fetches through its start time and rereads seven days before the last successful cutoff. Rolling periods move with the update and custom periods keep their dates. Use `-refresh-from` to extend the scan for older imports within the archive period.

Updates merge by event ID and use the newest record when the API returns an existing ID. Records absent from an update are retained within the archive period. A new export can capture deletions on the server. A successful scan switches the archive to a new snapshot. If a scan stops before publication, the previous snapshot stays readable and `-resume DIR` continues the pending scan.

The archive root exposes the latest files. Resolve `DIR/current` once to read several files from one snapshot during an update. The current `pages.jsonl` contains the latest scan, while `streams.jsonl` contains the merged history. Earlier snapshots retain their journals and use local disk space. `manifest.json` includes the successful cutoff and missing catalog IDs.

Use `-import-history` to create a new archive from a v1 or v2 journal with complete general history. It copies the general history and available catalog metadata into a new directory. Later updates can fetch missing catalog records.

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

Each export gets its own directory. Keep `session.json` and `pages.jsonl` to resume a stats.fm export. New sessions use format v3. Full v3 exports read up to 1000 general streams or 500 top tracks per page and run up to three requests at once. Sessions v1 and v2 resume with their original queries, page sizes, and file layout. When reading version 1 history, select rows with `method == "streams"` for one record per play.

Version 2 and later fetch the general history once and can contain fewer ready-made reports. A `reconstructible` entry in `manifest.json` gives the source file and query, the inclusive UTC bounds in Unix milliseconds, and, for an entity, the field that identifies it. A missing lower bound denotes all available history up to the given upper bound. The other statuses distinguish empty, private, unavailable, and incomplete results. The exporter fetches the original report when history coverage or required fields are missing.

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

Run checks with `go test -race ./...` and `go vet ./...`. The stats.fm tests include anonymized API responses for the same closed period, validate the recovery rules, and compare request counts and output across versions, modes, and worker counts on identical input.
