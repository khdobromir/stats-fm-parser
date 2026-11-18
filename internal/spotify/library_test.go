package spotify

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func spotifyZIP(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Архив аккаунта.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for name, body := range entries {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSpotifyLibraryExport(t *testing.T) {
	const library = `{"tracks":[
		{"track":"Кукушка","artist":"Кино","album":"Чёрный альбом","uri":"spotify:track:123"},
		{"track":"音楽","artist":null},
		{"track":"Кукушка","artist":"Кино","album":"Чёрный альбом","uri":"spotify:track:123"}
	],"episodes":[{"track":"ignored"}],"albums":[{"album":"ignored"}]}`
	const expected = "{\"title\":\"Кукушка\",\"artist\":\"Кино\",\"album\":\"Чёрный альбом\",\"uri\":\"spotify:track:123\"}\n" +
		"{\"title\":\"音楽\",\"artist\":null,\"album\":null,\"uri\":null}\n" +
		"{\"title\":\"Кукушка\",\"artist\":\"Кино\",\"album\":\"Чёрный альбом\",\"uri\":\"spotify:track:123\"}\n"
	jsonFile := filepath.Join(t.TempDir(), "Моя библиотека.json")
	if err := os.WriteFile(jsonFile, []byte(library), 0600); err != nil {
		t.Fatal(err)
	}
	zipFile := spotifyZIP(t, map[string]string{
		"My Spotify Account Data/YourLibrary.json": library,
		"Userdata.json": "ignored",
	})
	for _, source := range []string{jsonFile, zipFile} {
		file, count, err := Export(context.Background(), source, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if count != 3 || filepath.Base(file) != spotifyTracksFile {
			t.Fatal(file, count)
		}
		if actual := string(readOutput(t, filepath.Dir(file), spotifyTracksFile)); actual != expected {
			t.Fatalf("unexpected JSONL\n%s", actual)
		}
	}
}

func TestSpotifyLibraryEmptyAndErrors(t *testing.T) {
	source := filepath.Join(t.TempDir(), "YourLibrary.json")
	out := t.TempDir()
	for _, body := range []string{
		`{}`, `{"tracks":null}`, `{"tracks":{}}`, `{"tracks":[null]}`, `{"tracks":[}`, `{"tracks":[]}null`,
	} {
		if err := os.WriteFile(source, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if file, _, err := Export(context.Background(), source, out); err == nil || file != "" {
			t.Fatalf("accepted invalid library %s", body)
		}
	}
	for _, path := range []string{
		spotifyZIP(t, map[string]string{"Other.json": "{}"}),
		spotifyZIP(t, map[string]string{"one/YourLibrary.json": "{}", "two/YourLibrary.json": "{}"}),
	} {
		if _, _, err := Export(context.Background(), path, out); err == nil {
			t.Fatalf("accepted invalid archive %s", path)
		}
	}
	if err := os.WriteFile(source, []byte(`{"tracks":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Export(ctx, source, out); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
		t.Fatalf("unexpected files: %v, %v", entries, err)
	}
	file, count, err := Export(context.Background(), source, out)
	if err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if len(readOutput(t, filepath.Dir(file), spotifyTracksFile)) != 0 {
		t.Fatal("empty library must produce empty JSONL")
	}
}

func readOutput(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
