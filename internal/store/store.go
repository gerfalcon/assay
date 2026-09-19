// Package store is the append-only, file-backed history.
//
// No database, and not only because the constraint said so. The data has two
// shapes and neither wants a relational store: measurements and findings are an
// append-only time series (never updated, always read as a range — that is a
// log, not a table), and verdicts are a small key-value map.
//
// Four design rules:
//
//  1. Raw data is the truth; rollups are a cache. If a rollup is wrong, delete
//     and rebuild. There is no state that cannot be recomputed, which is the
//     actual reason no database is needed.
//  2. Partition by date, so a range query is a directory listing. No index.
//  3. Verdicts are an append-only log, not a mutable record. The audit trail
//     comes free and last-write-wins needs no transactions.
//  4. Every file is readable with grep and jq, without our binary.
//
// Scale check: a 5-year service is ~2,900 commits x ~15 project metrics ~= 44k records,
// about 4 MB. Five repos over five years with file-level detail lands in
// the low hundreds of MB. A linear scan of a date-partitioned subset is
// milliseconds. If that ever stops being true the upgrade is Parquet or an
// embedded KV as an INDEX over unchanged JSONL — the on-disk contract does not
// move.
package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sherzing/assay/pkg/schema"
)

// Store is a directory. That is the whole abstraction.
type Store struct{ Root string }

func Open(root string) (*Store, error) {
	if root == "" {
		root = ".assay"
	}
	for _, d := range []string{"measures", "findings", "verdicts", "tickets", "rollup"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{Root: root}, nil
}

// dayPath returns the partition file for a timestamp: measures/2026/09/19.jsonl
func (s *Store) dayPath(kind string, ts time.Time) string {
	u := ts.UTC()
	return filepath.Join(s.Root, kind,
		fmt.Sprintf("%04d", u.Year()), fmt.Sprintf("%02d", int(u.Month())),
		fmt.Sprintf("%02d.jsonl", u.Day()))
}

// Append writes records to their date partitions.
//
// Opens one handle per partition per call rather than per record — a history
// backfill writes thousands of records across a handful of days, and reopening
// per record would dominate the runtime.
func (s *Store) Append(r io.Reader) (measures, findings, verdicts int, err error) {
	files := map[string]*schema.Encoder{}
	handles := map[string]*os.File{}
	defer func() {
		for _, e := range files {
			_ = e.Flush()
		}
		for _, h := range handles {
			_ = h.Close()
		}
	}()

	out := func(path string) (*schema.Encoder, error) {
		if e, ok := files[path]; ok {
			return e, nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		h, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		handles[path] = h
		e := schema.NewEncoder(h)
		files[path] = e
		return e, nil
	}

	derr := schema.Decode(r, func(rec schema.Record) error {
		var path string
		var payload any
		switch {
		case rec.Measure != nil:
			if rec.Measure.TS.IsZero() {
				rec.Measure.TS = time.Now().UTC()
			}
			path, payload = s.dayPath("measures", rec.Measure.TS), rec.Measure
			measures++
		case rec.Finding != nil:
			if rec.Finding.TS.IsZero() {
				rec.Finding.TS = time.Now().UTC()
			}
			path, payload = s.dayPath("findings", rec.Finding.TS), rec.Finding
			findings++
		case rec.Verdict != nil:
			if rec.Verdict.TS.IsZero() {
				rec.Verdict.TS = time.Now().UTC()
			}
			// Verdicts are a single append-only log, not date-partitioned:
			// the whole file is read to resolve current state, and there are
			// thousands of them, not millions.
			path, payload = filepath.Join(s.Root, "verdicts", "verdicts.jsonl"), rec.Verdict
			verdicts++
		default:
			return nil
		}
		e, err := out(path)
		if err != nil {
			return err
		}
		return e.Write(payload)
	}, nil)
	return measures, findings, verdicts, derr
}

// Query filters the measurement series.
type Query struct {
	Repo   string
	Metric string
	Scope  schema.Scope
	Path   string
	Since  time.Time
	Until  time.Time
}

func (q Query) match(m *schema.Measure) bool {
	if q.Repo != "" && m.Repo != q.Repo {
		return false
	}
	if q.Metric != "" && m.Metric != q.Metric {
		return false
	}
	if q.Scope != "" && m.Scope != q.Scope {
		return false
	}
	if q.Path != "" && !strings.HasPrefix(m.Path, q.Path) {
		return false
	}
	if !q.Since.IsZero() && m.TS.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && m.TS.After(q.Until) {
		return false
	}
	return true
}

// QueryMeasures scans the relevant partitions and returns matches in time order.
//
// The date filter is applied to the *directory walk* before any file is opened,
// which is the entire performance story: asking for one month of one repo reads
// ~30 small files regardless of how many years the store holds.
func (s *Store) QueryMeasures(q Query) ([]schema.Measure, error) {
	var out []schema.Measure
	err := s.walkDays("measures", q.Since, q.Until, func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return nil // a partition that vanished mid-scan is not fatal
		}
		defer f.Close()
		return schema.Decode(f, func(rec schema.Record) error {
			if rec.Measure != nil && q.match(rec.Measure) {
				out = append(out, *rec.Measure)
			}
			return nil
		}, nil)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, err
}

// walkDays visits partition files whose date falls in range. Filtering on the
// path avoids opening files that cannot contain matches.
func (s *Store) walkDays(kind string, since, until time.Time, fn func(string) error) error {
	root := filepath.Join(s.Root, kind)
	var paths []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		if d, ok := dateFromPath(root, p); ok {
			if !since.IsZero() && d.Before(truncDay(since)) {
				return nil
			}
			if !until.IsZero() && d.After(until) {
				return nil
			}
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

func truncDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// dateFromPath recovers the partition date from .../2026/09/19.jsonl
func dateFromPath(root, p string) (time.Time, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return time.Time{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	t, err := time.Parse("2006/01/02", parts[0]+"/"+parts[1]+"/"+strings.TrimSuffix(parts[2], ".jsonl"))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Verdicts resolves the append-only log to current state: last write per
// fingerprint wins.
func (s *Store) Verdicts() (map[string]schema.Verdict, error) {
	out := map[string]schema.Verdict{}
	f, err := os.Open(filepath.Join(s.Root, "verdicts", "verdicts.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()
	err = schema.Decode(f, func(rec schema.Record) error {
		if v := rec.Verdict; v != nil {
			// Ordering is file order, which is append order. A later line for
			// the same fingerprint supersedes an earlier one.
			out[v.Fingerprint] = *v
		}
		return nil
	}, nil)
	return out, err
}

// Tickets reads the append-only ticket log. Callers resolve to current state
// with docket.Latest; this returns raw history so the audit trail is available.
func (s *Store) Tickets() ([]schema.Ticket, error) {
	var out []schema.Ticket
	f, err := os.Open(filepath.Join(s.Root, "tickets", "tickets.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()
	err = schema.Decode(f, func(rec schema.Record) error {
		if rec.Ticket != nil {
			out = append(out, *rec.Ticket)
		}
		return nil
	}, nil)
	return out, err
}

// AppendTicket records a ticket event. Append-only, like verdicts: a state
// change is a new line rather than an edit, so the history of who ticketed and
// closed what survives.
func (s *Store) AppendTicket(t schema.Ticket) error {
	dir := filepath.Join(s.Root, "tickets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "tickets.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := schema.NewEncoder(f)
	if err := enc.Write(&t); err != nil {
		return err
	}
	return enc.Flush()
}

// Bucket is one rolled-up period.
type Bucket struct {
	Repo   string    `json:"repo"`
	Metric string    `json:"metric"`
	Start  time.Time `json:"start"`
	N      int       `json:"n"`
	Min    float64   `json:"min"`
	Max    float64   `json:"max"`
	Mean   float64   `json:"mean"`
	Last   float64   `json:"last"`
}

// Rollup aggregates measurements into periods. Purely derived — safe to delete
// and rebuild, which is why it lives in its own directory.
//
// `Last` is usually the value you want for a trend: the state the codebase was
// left in at the end of the period, rather than an average over a period during
// which it changed.
func (s *Store) Rollup(q Query, period string) ([]Bucket, error) {
	ms, err := s.QueryMeasures(q)
	if err != nil {
		return nil, err
	}
	key := func(t time.Time) time.Time {
		u := t.UTC()
		switch period {
		case "day":
			return truncDay(u)
		case "month":
			return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		default: // week, ISO-style Monday start
			d := truncDay(u)
			return d.AddDate(0, 0, -(int(d.Weekday()+6) % 7))
		}
	}
	type k struct {
		repo, metric string
		start        time.Time
	}
	acc := map[k]*Bucket{}
	for _, m := range ms {
		kk := k{m.Repo, m.Metric, key(m.TS)}
		b, ok := acc[kk]
		if !ok {
			b = &Bucket{Repo: m.Repo, Metric: m.Metric, Start: kk.start, Min: m.Value, Max: m.Value}
			acc[kk] = b
		}
		b.N++
		b.Mean += (m.Value - b.Mean) / float64(b.N) // running mean, no overflow
		if m.Value < b.Min {
			b.Min = m.Value
		}
		if m.Value > b.Max {
			b.Max = m.Value
		}
		b.Last = m.Value // measures are time-sorted, so this ends as the period's final value
	}
	out := make([]Bucket, 0, len(acc))
	for _, b := range acc {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Metric < out[j].Metric
	})
	return out, nil
}
