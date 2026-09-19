// Command strata is the append-only history store for assay records.
//
//	… | strata append              read JSONL on stdin, write to date partitions
//	strata query --metric X        range query, JSONL on stdout
//	strata rollup --period week    aggregate to periods
//	strata verdicts                resolve the verdict log to current state
//	strata stat                    what is in the store
//
// Nothing here is quality-specific. Point it at any conforming JSONL stream and
// you get range queries back — which is the test of whether the decomposition
// is real rather than three entry points on a monolith.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sherzing/assay/internal/store"
	"github.com/sherzing/assay/pkg/schema"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "append":
		err = cmdAppend(os.Args[2:])
	case "query":
		err = cmdQuery(os.Args[2:])
	case "rollup":
		err = cmdRollup(os.Args[2:])
	case "verdicts":
		err = cmdVerdicts(os.Args[2:])
	case "stat":
		err = cmdStat(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("strata", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "strata: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "strata:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `strata — append-only history for assay records

usage:
  … | strata append [--store DIR] [--repo myservice]
  strata query    [--store DIR] [--repo N] [--metric M] [--scope S] [--since D] [--until D]
  strata rollup   [--period day|week|month] [same filters]
  strata verdicts [--store DIR] [--rule R]
  strata stat     [--store DIR]

dates are YYYY-MM-DD. default store is .assay
everything reads and writes JSONL, so it composes:
  ratchet scan . --emit measures | strata append --repo myservice
  strata query --repo myservice --metric cognitive.p90 | jq -r '[.ts,.value]|@csv'
`)
}

func storeFlag(fs *flag.FlagSet) *string {
	return fs.String("store", ".assay", "store directory")
}

func parseDay(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse("2006-01-02", s)
}

func cmdAppend(args []string) error {
	fs := flag.NewFlagSet("append", flag.ExitOnError)
	dir := storeFlag(fs)
	repo := fs.String("repo", "", "stamp records that carry no repo with this name")
	fs.Parse(args)

	s, err := store.Open(*dir)
	if err != nil {
		return err
	}

	in := os.Stdin
	// Stamping the repo rewrites the stream before it reaches the store, so
	// producers stay ignorant of where their output ends up.
	if *repo != "" {
		pr, pw, perr := os.Pipe()
		if perr != nil {
			return perr
		}
		go func() {
			defer pw.Close()
			enc := schema.NewEncoder(pw)
			defer enc.Flush()
			_ = schema.Decode(os.Stdin, func(rec schema.Record) error {
				switch {
				case rec.Measure != nil:
					if rec.Measure.Repo == "" {
						rec.Measure.Repo = *repo
					}
					return enc.Write(rec.Measure)
				case rec.Finding != nil:
					if rec.Finding.Repo == "" {
						rec.Finding.Repo = *repo
					}
					return enc.Write(rec.Finding)
				case rec.Verdict != nil:
					if rec.Verdict.Repo == "" {
						rec.Verdict.Repo = *repo
					}
					return enc.Write(rec.Verdict)
				}
				return nil
			}, nil)
		}()
		in = pr
	}

	m, f, v, err := s.Append(in)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "appended %d measures, %d findings, %d verdicts to %s\n", m, f, v, *dir)
	return nil
}

type filters struct {
	dir, repo, metric, scope, since, until *string
}

func queryFlags(fs *flag.FlagSet) filters {
	return filters{
		dir:    storeFlag(fs),
		repo:   fs.String("repo", "", "filter by repo"),
		metric: fs.String("metric", "", "filter by metric"),
		scope:  fs.String("scope", "", "project|module|file|function"),
		since:  fs.String("since", "", "YYYY-MM-DD"),
		until:  fs.String("until", "", "YYYY-MM-DD"),
	}
}

func (f filters) query() (store.Query, error) {
	sd, err := parseDay(*f.since)
	if err != nil {
		return store.Query{}, fmt.Errorf("--since: %w", err)
	}
	ud, err := parseDay(*f.until)
	if err != nil {
		return store.Query{}, fmt.Errorf("--until: %w", err)
	}
	if !ud.IsZero() {
		ud = ud.Add(24*time.Hour - time.Nanosecond) // --until includes that whole day
	}
	return store.Query{
		Repo: *f.repo, Metric: *f.metric, Scope: schema.Scope(*f.scope), Since: sd, Until: ud,
	}, nil
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	f := queryFlags(fs)
	format := fs.String("format", "jsonl", "jsonl|csv")
	fs.Parse(args)

	s, err := store.Open(*f.dir)
	if err != nil {
		return err
	}
	q, err := f.query()
	if err != nil {
		return err
	}
	ms, err := s.QueryMeasures(q)
	if err != nil {
		return err
	}
	if *format == "csv" {
		fmt.Println("ts,repo,commit,scope,path,metric,value")
		for _, m := range ms {
			fmt.Printf("%s,%s,%s,%s,%s,%s,%g\n",
				m.TS.UTC().Format(time.RFC3339), m.Repo, m.Commit, m.Scope, m.Path, m.Metric, m.Value)
		}
		return nil
	}
	enc := schema.NewEncoder(os.Stdout)
	defer enc.Flush()
	for i := range ms {
		if err := enc.Write(&ms[i]); err != nil {
			return err
		}
	}
	return nil
}

func cmdRollup(args []string) error {
	fs := flag.NewFlagSet("rollup", flag.ExitOnError)
	f := queryFlags(fs)
	period := fs.String("period", "week", "day|week|month")
	format := fs.String("format", "table", "table|jsonl|csv")
	fs.Parse(args)

	s, err := store.Open(*f.dir)
	if err != nil {
		return err
	}
	q, err := f.query()
	if err != nil {
		return err
	}
	bs, err := s.Rollup(q, *period)
	if err != nil {
		return err
	}
	switch *format {
	case "jsonl":
		e := json.NewEncoder(os.Stdout)
		for _, b := range bs {
			if err := e.Encode(b); err != nil {
				return err
			}
		}
	case "csv":
		fmt.Println("start,repo,metric,n,min,mean,max,last")
		for _, b := range bs {
			fmt.Printf("%s,%s,%s,%d,%g,%.4f,%g,%g\n",
				b.Start.Format("2006-01-02"), b.Repo, b.Metric, b.N, b.Min, b.Mean, b.Max, b.Last)
		}
	default:
		if len(bs) == 0 {
			fmt.Println("no data")
			return nil
		}
		fmt.Printf("%-12s %-16s %-22s %5s %8s %8s %8s\n", "start", "repo", "metric", "n", "min", "max", "last")
		for _, b := range bs {
			fmt.Printf("%-12s %-16s %-22s %5d %8g %8g %8g\n",
				b.Start.Format("2006-01-02"), trunc(b.Repo, 16), trunc(b.Metric, 22), b.N, b.Min, b.Max, b.Last)
		}
	}
	return nil
}

func cmdVerdicts(args []string) error {
	fs := flag.NewFlagSet("verdicts", flag.ExitOnError)
	dir := storeFlag(fs)
	rule := fs.String("rule", "", "filter by rule")
	fs.Parse(args)

	s, err := store.Open(*dir)
	if err != nil {
		return err
	}
	vs, err := s.Verdicts()
	if err != nil {
		return err
	}
	fps := make([]string, 0, len(vs))
	for fp := range vs {
		fps = append(fps, fp)
	}
	sort.Strings(fps) // map order is random; a stable stream is diffable

	counts := map[schema.Judgement]int{}
	enc := schema.NewEncoder(os.Stdout)
	defer enc.Flush()
	for _, fp := range fps {
		v := vs[fp]
		if *rule != "" && v.Rule != *rule {
			continue
		}
		counts[v.Verdict]++
		if err := enc.Write(&v); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "%d accepted, %d false-positive, %d wont-fix\n",
		counts[schema.Accepted], counts[schema.FalsePositive], counts[schema.WontFix])
	return nil
}

func cmdStat(args []string) error {
	fs := flag.NewFlagSet("stat", flag.ExitOnError)
	dir := storeFlag(fs)
	fs.Parse(args)

	s, err := store.Open(*dir)
	if err != nil {
		return err
	}
	ms, err := s.QueryMeasures(store.Query{})
	if err != nil {
		return err
	}
	repos := map[string]int{}
	metrics := map[string]int{}
	var first, last time.Time
	for _, m := range ms {
		repos[m.Repo]++
		metrics[m.Metric]++
		if first.IsZero() || m.TS.Before(first) {
			first = m.TS
		}
		if m.TS.After(last) {
			last = m.TS
		}
	}
	vs, _ := s.Verdicts()
	fmt.Printf("store:    %s\n", s.Root)
	fmt.Printf("measures: %d", len(ms))
	if len(ms) > 0 {
		fmt.Printf("  %s → %s", first.Format("2006-01-02"), last.Format("2006-01-02"))
	}
	fmt.Printf("\nverdicts: %d\n", len(vs))
	if len(repos) > 0 {
		fmt.Printf("repos:    %s\n", strings.Join(sortedKeys(repos), ", "))
		fmt.Printf("metrics:  %s\n", strings.Join(sortedKeys(metrics), ", "))
	}
	return nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
