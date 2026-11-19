package statsfm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func importArchive(ctx context.Context, o Options, report func(Update)) (string, error) {
	source := o.ImportHistory
	lock, err := lockDirectory(source, syscall.LOCK_SH)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	original, err := readConfig(source)
	if err != nil {
		return "", err
	}
	if original.version() > 2 || original.Version < 0 || original.Window != nil &&
		(len(original.Periods) != 1 || original.Window.After > original.historyEnd()) {
		return "", errors.New("импорт требует выгрузку v1/v2 с корректным периодом")
	}
	journal, err := os.Open(filepath.Join(source, "pages.jsonl"))
	if err != nil {
		return "", err
	}
	defer journal.Close()

	if err := os.MkdirAll(o.Out, 0700); err != nil {
		return "", err
	}
	root, err := os.MkdirTemp(o.Out, "statsfm-history-")
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(root)
		}
	}()
	archiveLock, err := lockDirectory(root, syscall.LOCK_EX)
	if err != nil {
		return "", err
	}
	defer archiveLock.Close()
	cfg := original
	cfg.Version, cfg.Mode = currentVersion, HistoryMode
	if err := openArchiveWork(root, cfg, nil); err != nil {
		return "", err
	}
	work, err := openStore(filepath.Join(root, pendingArchive), nil)
	if err != nil {
		return "", err
	}
	defer work.close()

	data := newArchiveData()
	idx := newIndex()
	encoder := json.NewEncoder(work.journal)
	_, err = walkPages(journal, func(p page) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case p.Query == original.historyQuery(), p.Query.metadata(), p.Query.Path == "user":
			if err := data.add(p, cfg); err != nil {
				return err
			}
			idx.apply(p)
			return encoder.Encode(p)
		case p.Query.Path == "top/tracks" || p.Query.Path == "top/artists" || p.Query.Path == "top/albums":
			kind := strings.TrimPrefix(p.Query.Path, "top/")
			cache := newIndex()
			var accepted []any
			for _, row := range p.Rows {
				cache.noteRankingMetadata(p.Query, row)
				item, _ := row.(object)
				entityRow, _ := item[strings.TrimSuffix(kind, "s")].(object)
				e := entity{kind, entityID(entityRow["id"])}
				if cache.metadata[e] {
					if _, exists := data.metadata[e]; !exists {
						accepted = append(accepted, entityRow)
					}
				}
			}
			if len(accepted) != 0 {
				catalog := page{Query: query{Path: kind + "/list"}, FetchedAt: p.FetchedAt, Rows: accepted, Status: "complete"}
				if err := data.add(catalog, cfg); err != nil {
					return err
				}
				return encoder.Encode(p)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !historyComplete(idx.results[original.historyQuery()]) {
		return "", errors.New("в исходной выгрузке нет полной общей истории")
	}
	for e := range data.metadata {
		idx.metadata[e] = true
	}
	if err := work.journal.Sync(); err != nil {
		return "", err
	}
	state := archiveState(idx)
	if err := publishArchive(ctx, root, work, data, idx, state); err != nil {
		if current, _ := snapshotName(root); current != "" {
			keep = true
			return root, err
		}
		return "", err
	}
	keep = true
	report(Update{Dir: root, State: state, Progress: Progress{Records: len(data.streams), Streams: len(data.streams)}})
	return root, nil
}
