package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

type store struct {
	dir           string
	cfg           config
	lock, journal *os.File
}

func atomicFile(dir, name string, write func(*os.File) error) error {
	f, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = write(f)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}
func writeJSON(dir, name string, v any) error {
	return atomicFile(dir, name, func(f *os.File) error { return json.NewEncoder(f).Encode(v) })
}

func openStore(dir string, cfg *config) (*store, error) {
	lock, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("export directory is locked: %w", err)
	}
	s := &store{dir: dir, lock: lock}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	if cfg != nil {
		s.cfg = *cfg
		flags |= os.O_CREATE | os.O_EXCL
		err = writeJSON(dir, "session.json", s.cfg)
	} else {
		var b []byte
		b, err = os.ReadFile(filepath.Join(dir, "session.json"))
		if err == nil {
			err = json.Unmarshal(b, &s.cfg)
		}
		if err == nil && (!safeSegment(s.cfg.Profile.ID) || s.cfg.Started.IsZero() || len(s.cfg.Periods) == 0) {
			err = errors.New("invalid export session")
		}
	}
	if err != nil {
		return nil, err
	}
	s.journal, err = os.OpenFile(filepath.Join(dir, "pages.jsonl"), flags, 0600)
	if err != nil {
		return nil, err
	}
	return s, nil
}
func (s *store) close() {
	if s.journal != nil {
		s.journal.Close()
	}
	s.lock.Close()
}
func (s *store) append(p page) error {
	p.FetchedAt = time.Now().UTC()
	if err := json.NewEncoder(s.journal).Encode(p); err != nil {
		return err
	}
	return s.journal.Sync()
}

// A newline commits a page. After an interrupted write, replay only complete pages.
func (s *store) replay(visit func(page) error) error {
	if _, err := s.journal.Seek(0, io.SeekStart); err != nil {
		return err
	}
	rd := bufio.NewReader(s.journal)
	var offset int64
	for {
		b, err := rd.ReadBytes('\n')
		if err == io.EOF {
			if len(b) > 0 {
				if err = s.journal.Truncate(offset); err != nil {
					return err
				}
			}
			_, err = s.journal.Seek(0, io.SeekEnd)
			return err
		}
		if err != nil {
			return err
		}
		var p page
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if !json.Valid(b) {
			return fmt.Errorf("corrupt journal at byte %d", offset)
		}
		if err = dec.Decode(&p); err != nil {
			return err
		}
		if err = visit(p); err != nil {
			return err
		}
		offset += int64(len(b))
	}
}

type summaryGroup struct {
	Query  query    `json:"query"`
	Status string   `json:"status"`
	Rows   []object `json:"rows"`
}

func (s *store) publish(idx *index, state string, reason error) error {
	groups := map[query]*summaryGroup{}
	totals := map[string]any{}
	counts := map[string]int{}
	// Publish the manifest last so that it marks the finished set of files.
	stage, err := os.MkdirTemp(s.dir, ".publish-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	files := map[string]*os.File{}
	writers := map[string]*bufio.Writer{}
	encoders := map[string]*json.Encoder{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for _, name := range []string{"rankings", "streams", "statistics", "recaps"} {
		f, err := os.OpenFile(filepath.Join(stage, name+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		files[name] = f
		writers[name] = bufio.NewWriter(f)
		encoders[name] = json.NewEncoder(writers[name])
		counts[name] = 0
	}
	err = s.replay(func(p page) error {
		q := p.Query
		name := q.file()
		general := name == "rankings" && strings.Count(q.Path, "/") == 1
		if general && groups[q] == nil {
			groups[q] = &summaryGroup{Query: q, Rows: []object{}}
		}
		if general {
			groups[q].Status = p.Status
		}
		for _, row := range p.Rows {
			if general && len(groups[q].Rows) < 20 {
				groups[q].Rows = append(groups[q].Rows, compactRanking(q, row))
			}
			if name == "rankings" {
				if o, ok := row.(object); ok {
					for _, field := range []string{"streams", "playedMs"} {
						if _, has := o[field]; !has {
							o[field] = nil
						}
					}
				}
			}
			if q.Path == "streams/stats" {
				totals[q.Period] = row
			}
			record := struct {
				FetchedAt time.Time `json:"fetched_at"`
				Type      string    `json:"type"`
				User      string    `json:"user_id"`
				query
				From string `json:"from,omitempty"`
				To   string `json:"to,omitempty"`
				Data any    `json:"data"`
			}{FetchedAt: p.FetchedAt, Type: name, User: s.cfg.Profile.ID, query: q, Data: row}
			if q.Period == "custom" {
				record.From = s.cfg.From
				record.To = s.cfg.To
			}
			if err := encoders[name].Encode(record); err != nil {
				return err
			}
			counts[name]++
		}
		return nil
	})
	if err != nil {
		return err
	}
	for name, f := range files {
		if err := writers[name].Flush(); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		delete(files, name)
		filename := name + ".jsonl"
		if err := os.Rename(filepath.Join(stage, filename), filepath.Join(s.dir, filename)); err != nil {
			return err
		}
	}
	rankings := []*summaryGroup{}
	results := []*result{}
	coverage := map[string]int{}
	for q, r := range idx.results {
		results = append(results, r)
		coverage[r.Status]++
		if g := groups[q]; g != nil {
			rankings = append(rankings, g)
		}
	}
	cmp := func(a, b query) int {
		if d := strings.Compare(a.Path, b.Path); d != 0 {
			return d
		}
		if d := strings.Compare(a.Period, b.Period); d != 0 {
			return d
		}
		return strings.Compare(a.Order, b.Order)
	}
	slices.SortFunc(rankings, func(a, b *summaryGroup) int { return cmp(a.Query, b.Query) })
	slices.SortFunc(results, func(a, b *result) int { return cmp(a.Query, b.Query) })
	ended := time.Now().UTC()
	if state == "finished" && (coverage["partial"] > 0 || coverage["failed"] > 0 || coverage["private"] > 0 || coverage["unavailable"] > 0) {
		state = "finished_with_gaps"
	}
	if err := writeJSON(s.dir, "summary.json", object{"profile": s.cfg.Profile, "started_at": s.cfg.Started, "exported_at": ended, "periods": s.cfg.Periods, "from": s.cfg.From, "to": s.cfg.To, "state": state, "coverage": coverage, "totals": totals, "rankings": rankings}); err != nil {
		return err
	}
	manifest := object{"version": 1, "session": s.cfg, "state": state, "finished_at": ended, "coverage": coverage, "rows": counts, "queries": results}
	if reason != nil {
		manifest["reason"] = reason.Error()
	}
	return writeJSON(s.dir, "manifest.json", manifest)
}
