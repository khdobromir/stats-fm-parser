package statsfm

import (
	"context"
	"fmt"
)

type fetchedPage struct {
	page
	err error
}

func collect(ctx context.Context, c *client, s *store, idx *index, notify func(Progress)) error {
	cfg := s.cfg
	workers := 1
	if cfg.version() >= 3 {
		workers = 3
		if c.workers != 0 {
			workers = c.workers
		}
	}

	run := func(queue []query, done int, parallelism int) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		pages := make(chan fetchedPage, parallelism)
		pending := map[query]bool{}
		var failure error
		journalFailed := false
		save := func(p page) {
			if err := s.append(p); err != nil {
				failure = err
				journalFailed = true
				cancel()
				return
			}
			idx.apply(p)
		}
		for len(queue) > 0 || len(pending) > 0 {
			for len(queue) > 0 && len(pending) < parallelism && ctx.Err() == nil {
				q := queue[0]
				queue = queue[1:]
				r := idx.results[q]
				if pending[q] || r != nil && r.Status != "running" {
					continue
				}
				if r == nil {
					r = &result{Query: q}
				}
				progress := Progress{Path: q.Path, Period: q.Period, Order: q.Order, Records: idx.count, Done: done, Total: len(idx.entities)}
				if history := idx.results[cfg.historyQuery()]; history != nil {
					progress.Streams = history.Count
				}
				notify(progress)
				if source := idx.reconstruct(cfg, q); source != nil {
					save(page{Query: q, Status: "reconstructible", Source: source})
					continue
				}
				pending[q] = true
				go func() {
					p, err := c.fetch(ctx, cfg, q, r)
					pages <- fetchedPage{p, err}
				}()
			}
			if len(pending) == 0 {
				break
			}
			fetched := <-pages
			q := fetched.Query
			delete(pending, q)
			if fetched.err != nil {
				if failure == nil {
					failure = fmt.Errorf("%s: %w", q.Path, fetched.err)
					cancel()
				}
				continue
			}
			// Save pages already received before cancellation, except after a write error.
			if !journalFailed {
				save(fetched.page)
			}
			if fetched.Status == "running" && ctx.Err() == nil {
				queue = append(queue, q)
			}
		}
		if failure != nil {
			return failure
		}
		return ctx.Err()
	}

	if cfg.version() >= 2 {
		if err := run([]query{{Path: "user"}}, 0, 1); err != nil {
			return err
		}
	}
	if cfg.version() >= 3 {
		if err := run([]query{cfg.historyQuery()}, 0, 1); err != nil {
			return err
		}
	}
	runReports := func(reports []query, done int) error {
		if cfg.version() >= 3 {
			return run(reports, done, workers)
		}
		for _, q := range reports {
			if err := run([]query{q}, done, 1); err != nil {
				return err
			}
		}
		return nil
	}
	if cfg.Mode == HistoryMode {
		history := idx.results[cfg.historyQuery()]
		if history == nil {
			return fmt.Errorf("история отсутствует в журнале")
		}
		if !historyComplete(history) {
			return fmt.Errorf("неполная история: %s", history.Status)
		}
	}

	if cfg.Mode != HistoryMode {
		if err := runReports(queries(cfg, entity{}), 0); err != nil {
			return err
		}
		for n := 0; n < len(idx.entities); {
			end := min(n+workers, len(idx.entities))
			var reports []query
			for _, e := range idx.entities[n:end] {
				reports = append(reports, queries(cfg, e)...)
			}
			if err := runReports(reports, n); err != nil {
				return err
			}
			n = end
		}
	}
	if cfg.version() >= 2 {
		if err := runReports(idx.metadataBatches(), len(idx.entities)); err != nil {
			return err
		}
		var singles []query
		for _, e := range idx.entities {
			if !idx.metadata[e] {
				singles = append(singles, query{Path: e.kind + "/" + e.id})
			}
		}
		return runReports(singles, len(idx.entities))
	}
	return nil
}
