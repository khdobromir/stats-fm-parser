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
| `rankings.jsonl`, `streams.jsonl`, `statistics.jsonl`, `recaps.jsonl` | Detailed stats.fm results |
| `manifest.json` | Counts and coverage, including unavailable or partial results |
| `spotify_liked_tracks.jsonl` | Spotify track titles, artists, albums, and URIs in archive order |

Each export gets its own directory. Keep `session.json` and `pages.jsonl` to resume a stats.fm export. For an individual listening history, select rows with `method == "streams"` in `streams.jsonl` to avoid counting track, artist, and album views of the same play.

## Development

- `internal/app` handles CLI flags, the terminal UI, and export selection.
- `internal/statsfm` handles API requests, export sessions, and resume.
- `internal/spotify` imports Liked Songs from account archives.
- `internal/exportfs` provides atomic file writes shared by both exporters.

Run checks with `go test -race ./...` and `go vet ./...`.
