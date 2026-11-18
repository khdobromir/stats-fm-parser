package spotify

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"stats-fm-parser/internal/exportfs"
)

const spotifyTracksFile = "spotify_liked_tracks.jsonl"

type spotifyTrack struct {
	Title  *string `json:"track"`
	Artist *string `json:"artist"`
	Album  *string `json:"album"`
	URI    *string `json:"uri"`
}

func spotifyLibraryEntry(f *os.File, size int64) (io.ReadCloser, error) {
	archive, err := zip.NewReader(f, size)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать ZIP Spotify: %w", err)
	}
	var library *zip.File
	for _, entry := range archive.File {
		if path.Base(entry.Name) != "YourLibrary.json" || !entry.Mode().IsRegular() {
			continue
		}
		if library != nil {
			return nil, errors.New("в архиве несколько файлов YourLibrary.json")
		}
		library = entry
	}
	if library == nil {
		return nil, errors.New("в архиве нет YourLibrary.json. Выберите пакет Account data")
	}
	return library.Open()
}

func readSpotifyTracks(source string) ([]*spotifyTrack, error) {
	info, err := os.Stat(source)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("выберите файл ZIP или YourLibrary.json")
	}
	f, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var library io.Reader = f
	switch strings.ToLower(filepath.Ext(source)) {
	case ".json":
	case ".zip":
		entry, err := spotifyLibraryEntry(f, info.Size())
		if err != nil {
			return nil, err
		}
		defer entry.Close()
		library = entry
	default:
		return nil, errors.New("выберите файл ZIP или YourLibrary.json")
	}

	const maxLibrarySize = 64 << 20
	data, err := io.ReadAll(io.LimitReader(library, maxLibrarySize+1))
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать библиотеку Spotify: %w", err)
	}
	if len(data) > maxLibrarySize {
		return nil, errors.New("YourLibrary.json превышает 64 МиБ")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("YourLibrary.json должен быть в кодировке UTF-8")
	}
	var saved struct {
		Tracks []*spotifyTrack `json:"tracks"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &saved); err != nil {
		return nil, fmt.Errorf("не удалось разобрать YourLibrary.json: %w", err)
	}
	if saved.Tracks == nil {
		return nil, errors.New("в YourLibrary.json отсутствует массив tracks")
	}
	for i, row := range saved.Tracks {
		if row == nil {
			return nil, fmt.Errorf("запись %d в tracks должна быть объектом", i+1)
		}
	}
	return saved.Tracks, nil
}

func Export(ctx context.Context, source, out string) (string, int, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	tracks, err := readSpotifyTracks(source)
	if err != nil {
		return "", 0, err
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		return "", 0, err
	}
	dir, err := os.MkdirTemp(out, "spotify-"+time.Now().UTC().Format("20060102-150405")+"-")
	if err != nil {
		return "", 0, err
	}
	err = exportfs.AtomicFile(dir, spotifyTracksFile, func(f *os.File) error {
		buffer := bufio.NewWriter(f)
		enc := json.NewEncoder(buffer)
		for _, track := range tracks {
			if err := ctx.Err(); err != nil {
				return err
			}
			row := struct {
				Title  *string `json:"title"`
				Artist *string `json:"artist"`
				Album  *string `json:"album"`
				URI    *string `json:"uri"`
			}{track.Title, track.Artist, track.Album, track.URI}
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		if err := buffer.Flush(); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return "", 0, errors.Join(err, os.Remove(dir))
	}
	return filepath.Join(dir, spotifyTracksFile), len(tracks), nil
}
