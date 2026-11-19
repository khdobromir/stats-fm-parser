package statsfm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"stats-fm-parser/internal/exportfs"
)

const pendingArchive = ".pending"
const currentSnapshot = "current"
const snapshotPrefix = ".snapshot-"

func isArchive(dir string) bool {
	for _, name := range []string{pendingArchive, currentSnapshot} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func historyComplete(r *result) bool {
	return r != nil && (r.Status == "complete" || r.Status == "empty")
}

func snapshotName(root string) (string, error) {
	name, err := os.Readlink(filepath.Join(root, currentSnapshot))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err == nil && (!safeSegment(name) || !strings.HasPrefix(name, snapshotPrefix)) {
		err = errors.New("некорректный указатель на снимок архива")
	}
	return name, err
}

func openArchiveWork(root string, cfg config, raw json.RawMessage) error {
	dir, err := os.MkdirTemp(root, ".work-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	s, err := openStore(dir, &cfg)
	if err != nil {
		return err
	}
	if raw != nil {
		q := query{Path: "user"}
		rows, parseErr := rowsFor(q, decodeResponse(raw), cfg)
		err = parseErr
		if err == nil {
			err = s.append(page{Query: q, Rows: rows, Response: raw, Status: "complete"})
		}
	}
	if err == nil {
		err = s.journal.Sync()
	}
	s.close()
	if err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	if err := os.Rename(dir, filepath.Join(root, pendingArchive)); err != nil {
		return err
	}
	return syncDir(root)
}

func updateConfig(base config, name, refresh string, started time.Time) (config, error) {
	cfg := base
	cfg.Started, cfg.Base = started.UTC(), name
	before := cfg.Started.UnixMilli()
	const week = int64(7 * 24 * time.Hour / time.Millisecond)
	after := base.historyEnd() - week
	if refresh != "" {
		date, err := time.Parse(time.DateOnly, refresh)
		if err != nil {
			return cfg, err
		}
		after = min(after, date.UnixMilli())
	}
	if cfg.Window != nil {
		period := cfg.Periods[0]
		cfg.Window = (Options{Period: period, From: cfg.From, To: cfg.To}).exportConfig(cfg.Profile, started).Window
		after = max(after, cfg.Window.After)
		before = min(before, cfg.Window.Before)
	}
	if before < base.historyEnd() || after > before {
		return cfg, errors.New("границы обновления не согласуются с предыдущей синхронизацией")
	}
	cfg.Scan = &dateWindow{After: after, Before: before}
	return cfg, nil
}

func (c *client) archive(ctx context.Context, root string, o Options, initial *config, raw json.RawMessage, report func(Update)) (string, error) {
	lock, err := lockDirectory(root, syscall.LOCK_EX)
	if err != nil {
		return root, err
	}
	defer lock.Close()
	current, err := snapshotName(root)
	if err != nil {
		return root, err
	}
	pending := filepath.Join(root, pendingArchive)
	if info, statErr := os.Lstat(pending); statErr == nil {
		if !info.IsDir() {
			return root, errors.New("рабочий каталог архива повреждён")
		}
		work, err := readConfig(pending)
		if err != nil {
			return root, err
		}
		if current != work.Base {
			published, err := readConfig(filepath.Join(root, current))
			if err != nil || !published.Started.Equal(work.Started) || published.Base != work.Base {
				return root, errors.New("рабочий журнал принадлежит другому снимку")
			}
			// The pointer was committed before the previous process stopped.
			if err := os.RemoveAll(pending); err != nil {
				return root, err
			}
		} else if o.RefreshFrom != "" {
			return root, errors.New("для завершения начатого обновления используйте -resume")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return root, statErr
	}

	if _, err := os.Stat(pending); errors.Is(err, os.ErrNotExist) {
		switch {
		case initial != nil:
			err = openArchiveWork(root, *initial, raw)
			if err != nil {
				return root, err
			}
		case o.Update != "" && current != "":
			base, err := readConfig(filepath.Join(root, current))
			if err != nil {
				return root, err
			}
			if base.Mode != HistoryMode || base.Version != currentVersion {
				return root, errors.New("обновление требует архив истории v3")
			}
			cfg, err := updateConfig(base, current, o.RefreshFrom, time.Now())
			if err != nil {
				return root, err
			}
			if err := openArchiveWork(root, cfg, nil); err != nil {
				return root, err
			}
		case current != "":
			var manifest struct {
				State string
				Rows  map[string]int
			}
			b, err := os.ReadFile(filepath.Join(root, current, "manifest.json"))
			if err != nil {
				return root, err
			}
			if err := json.Unmarshal(b, &manifest); err != nil {
				return root, err
			}
			count := manifest.Rows["streams"]
			report(Update{Dir: root, State: manifest.State, Progress: Progress{Records: count, Streams: count}})
			return root, nil
		default:
			return root, errors.New("для обновления нужен завершённый архив истории. Используйте -import-history для экспорта v1/v2")
		}
	} else if err != nil {
		return root, err
	}

	s, err := openStore(pending, nil)
	if err != nil {
		return root, err
	}
	defer s.close()
	if s.cfg.Mode != HistoryMode || s.cfg.Base != current {
		return root, errors.New("рабочая сессия не соответствует архиву")
	}
	report(Update{Dir: root})
	data := newArchiveData()
	if current != "" {
		if err := data.load(filepath.Join(root, current), s.cfg); err != nil {
			return root, err
		}
	}
	idx := newIndex()
	for _, id := range data.order {
		idx.discoverRow(data.streams[id].Rows[0])
	}
	for e := range data.metadata {
		idx.metadata[e] = true
	}
	q := s.cfg.historyQuery()
	reset := func() {
		if r := idx.results[q]; r != nil {
			idx.count -= r.Count
		}
		delete(idx.results, q)
		idx.discovered = map[entity]bool{}
		idx.entities = nil
		for _, id := range data.order {
			idx.discoverRow(data.streams[id].Rows[0])
		}
	}
	apply := func(p page) error {
		if p.Query == q && p.Status == "retry" {
			reset()
		} else {
			idx.apply(p)
		}
		return nil
	}
	if err := s.replay(apply); err != nil {
		return root, err
	}
	if r := idx.results[q]; r != nil && r.Status != "running" && !historyComplete(r) {
		if err := s.append(page{Query: q, Status: "retry"}); err != nil {
			return root, err
		}
		reset()
	}
	var last Progress
	err = collect(ctx, c, s, idx, func(p Progress) { last = p; report(Update{Dir: root, Progress: p}) })
	if err != nil {
		report(Update{Dir: root, State: "interrupted", Progress: last})
		return root, err
	}
	if err := ctx.Err(); err != nil {
		report(Update{Dir: root, State: "interrupted", Progress: last})
		return root, err
	}
	if err := data.merge(s); err != nil {
		return root, err
	}
	last.Path, last.Records, last.Streams = "publish", len(data.streams), len(data.streams)
	report(Update{Dir: root, Progress: last})
	idx.discovered = map[entity]bool{}
	idx.entities = nil
	for _, id := range data.order {
		idx.discoverRow(data.streams[id].Rows[0])
	}
	state := archiveState(idx)
	if err := publishArchive(ctx, root, s, data, idx, state); err != nil {
		report(Update{Dir: root, State: "save_error", Progress: last})
		return root, err
	}
	report(Update{Dir: root, State: state, Progress: last})
	return root, nil
}

func archiveState(idx *index) string {
	for _, e := range idx.entities {
		if !idx.metadata[e] {
			return "finished_with_gaps"
		}
	}
	return "finished"
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func publishArchive(ctx context.Context, root string, work *store, data *archiveData, idx *index, state string) error {
	dir, err := os.MkdirTemp(root, snapshotPrefix)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(dir)
		}
	}()
	if err := exportfs.WriteJSON(dir, "session.json", work.cfg); err != nil {
		return err
	}
	if _, err := work.journal.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := exportfs.AtomicFile(dir, "pages.jsonl", func(f *os.File) error {
		_, err := io.Copy(f, work.journal)
		return err
	}); err != nil {
		return err
	}
	s := &store{dir: dir, cfg: work.cfg}
	if err := s.publishFrom(idx, state, nil, data.rows); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	for _, name := range []string{"session.json", "pages.jsonl", "manifest.json", "summary.json", "streams.jsonl", "metadata.jsonl", "statistics.jsonl", "rankings.jsonl", "recaps.jsonl"} {
		path, target := filepath.Join(root, name), filepath.Join(currentSnapshot, name)
		if err := os.Symlink(target, path); err != nil {
			if existing, readErr := os.Readlink(path); readErr != nil || existing != target {
				return err
			}
		}
	}
	if err := syncDir(root); err != nil {
		return err
	}
	link := filepath.Join(dir, ".pointer")
	if err := os.Symlink(filepath.Base(dir), link); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(link, filepath.Join(root, currentSnapshot)); err != nil {
		return err
	}
	published = true
	if err := syncDir(root); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(root, pendingArchive))
}
