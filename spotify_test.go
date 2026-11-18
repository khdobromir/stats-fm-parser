package main

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

func TestSpotifyForm(t *testing.T) {
	m := ui{width: 40, height: 15}
	m.Update(tea.PasteMsg{Content: "volt"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if view := m.View().Content; !strings.Contains(view, "Любимые треки") || !strings.Contains(view, "Путь") {
		t.Fatal(view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if view := m.View().Content; m.running || !strings.Contains(view, "Укажите путь") {
		t.Fatal(view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.o.user != "volt" || !strings.Contains(m.View().Content, "Пользователь") {
		t.Fatal("draft lost when switching sources")
	}
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
		var final update
		dir, err := export(context.Background(), nil, options{spotifyLibrary: source, out: t.TempDir()}, func(u update) { final = u })
		if err != nil {
			t.Fatal(err)
		}
		if final.p.Records != 3 || final.state != "finished" || final.file != filepath.Join(dir, spotifyTracksFile) {
			t.Fatal(final)
		}
		if actual := string(readOutput(t, dir, spotifyTracksFile)); actual != expected {
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
		if file, _, err := exportSpotify(context.Background(), source, out); err == nil || file != "" {
			t.Fatalf("accepted invalid library %s", body)
		}
	}
	for _, path := range []string{
		spotifyZIP(t, map[string]string{"Other.json": "{}"}),
		spotifyZIP(t, map[string]string{"one/YourLibrary.json": "{}", "two/YourLibrary.json": "{}"}),
	} {
		if _, _, err := exportSpotify(context.Background(), path, out); err == nil {
			t.Fatalf("accepted invalid archive %s", path)
		}
	}
	if err := os.WriteFile(source, []byte(`{"tracks":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := exportSpotify(ctx, source, out); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
		t.Fatalf("unexpected files: %v, %v", entries, err)
	}
	file, count, err := exportSpotify(context.Background(), source, out)
	if err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if len(readOutput(t, filepath.Dir(file), spotifyTracksFile)) != 0 {
		t.Fatal("empty library must produce empty JSONL")
	}
}
