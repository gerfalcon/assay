// Command lens makes an assay stream readable by a person.
//
//	lens top      what is worst right now
//	lens trend    how a metric moved, as a sparkline
//	lens diff     what changed between two points
//	lens compare  the same metric across repos
//
// It reads JSONL on stdin, so it works on any conforming stream — not just one
// that came out of strata. That is the test of whether this is a real tool or a
// strata subcommand wearing a disguise:
//
//	strata query --repo service-a --metric cognitive | lens top
//	ratchet scan . --emit measures | lens top --metric cognitive
//
// grep and jq can do all of this. They just cannot do it at a glance, and the
// point of this tool is the glance.
package main

import (
	"flag"
	"fmt"
	"math"
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
	case "top":
		err = cmdTop(os.Args[2:])
	case "trend":
		err = cmdTrend(os.Args[2:])
	case "diff":
		err = cmdDiff(os.Args[2:])
	case "compare":
		err = cmdCompare(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("lens", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "lens: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lens:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lens — read an assay stream at a glance

usage:
  lens top     [--metric M] [--scope S] [--n 15] [--repo R]     worst right now
  lens trend   [--metric M] [--repo R] [--period week]          sparkline over time
  lens diff    --since DATE [--metric M] [--repo R]             what changed
  lens compare [--metric M] [--scope project]                   repos side by side

input: JSONL on stdin, or --store DIR to read a strata store

  strata query --repo service-a --metric cognitive | lens top
  ratchet scan . --emit measures | lens top --metric cognitive --n 10
  lens trend --store .assay --repo service-a --metric cognitive.p90
`)
}

// read takes measures from stdin or from a store, so every command works both
// as a pipe stage and standalone.
func read(dir string, q store.Query) ([]schema.Measure, error) {
	if dir != "" {
		s, err := store.Open(dir)
		if err != nil {
			return nil, err
		}
		return s.QueryMeasures(q)
	}
	st, _ := os.Stdin.Stat()
	if st != nil && st.Mode()&os.ModeCharDevice != 0 {
		return nil, fmt.Errorf("no input: pipe JSONL in, or pass --store DIR")
	}
	var out []schema.Measure
	err := schema.Decode(os.Stdin, func(rec schema.Record) error {
		if rec.Measure != nil {
			out = append(out, *rec.Measure)
		}
		return nil
	}, nil)
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, err
}

func inputFlags(fs *flag.FlagSet) (*string, *string, *string, *string) {
	return fs.String("store", "", "read a strata store instead of stdin"),
		fs.String("repo", "", "filter by repo"),
		fs.String("metric", "", "filter by metric"),
		fs.String("scope", "", "project|module|file|function")
}

func filter(ms []schema.Measure, repo, metric, scope string) []schema.Measure {
	var out []schema.Measure
	for _, m := range ms {
		if repo != "" && m.Repo != repo {
			continue
		}
		if metric != "" && m.Metric != metric {
			continue
		}
		if scope != "" && string(m.Scope) != scope {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ---------- top ----------

func cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	dir, repo, metric, scope := inputFlags(fs)
	n := fs.Int("n", 15, "how many")
	fs.Parse(args)
	if *metric == "" {
		*metric = "cognitive"
	}
	if *scope == "" {
		*scope = "function"
	}

	ms, err := read(*dir, store.Query{Repo: *repo, Metric: *metric, Scope: schema.Scope(*scope)})
	if err != nil {
		return err
	}
	ms = filter(ms, *repo, *metric, *scope)
	if len(ms) == 0 {
		return fmt.Errorf("no %s measures at scope %s", *metric, *scope)
	}

	// Latest value per path — a store holds history, and "worst right now"
	// means the most recent reading, not every reading ever taken.
	latest := map[string]schema.Measure{}
	for _, m := range ms {
		k := m.Repo + "\x00" + m.Path
		if cur, ok := latest[k]; !ok || m.TS.After(cur.TS) {
			latest[k] = m
		}
	}
	list := make([]schema.Measure, 0, len(latest))
	for _, m := range latest {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Value > list[j].Value })
	if len(list) > *n {
		list = list[:*n]
	}

	maxV := list[0].Value
	fmt.Printf("worst %d by %s (%s scope)\n\n", len(list), *metric, *scope)
	for _, m := range list {
		label := m.Path
		if *repo == "" {
			label = m.Repo + " " + label
		}
		fmt.Printf("%7s %s  %s\n", num(m.Value), bar(m.Value, maxV, 22), elide(label, 68))
	}
	return nil
}

// ---------- trend ----------

func cmdTrend(args []string) error {
	fs := flag.NewFlagSet("trend", flag.ExitOnError)
	dir, repo, metric, scope := inputFlags(fs)
	period := fs.String("period", "week", "day|week|month")
	fs.Parse(args)
	if *metric == "" {
		*metric = "cognitive.p90"
	}
	if *scope == "" {
		*scope = "project"
	}

	ms, err := read(*dir, store.Query{Repo: *repo, Metric: *metric, Scope: schema.Scope(*scope)})
	if err != nil {
		return err
	}
	ms = filter(ms, *repo, *metric, *scope)
	if len(ms) == 0 {
		return fmt.Errorf("no %s measures", *metric)
	}

	byRepo := map[string][]schema.Measure{}
	for _, m := range ms {
		byRepo[m.Repo] = append(byRepo[m.Repo], m)
	}
	fmt.Printf("%s over time, by %s\n\n", *metric, *period)
	for _, r := range sortedKeys(byRepo) {
		pts := bucketLast(byRepo[r], *period)
		if len(pts) == 0 {
			continue
		}
		vals := make([]float64, len(pts))
		for i, p := range pts {
			vals[i] = p.v
		}
		first, last := vals[0], vals[len(vals)-1]
		fmt.Printf("%-16s %s  %s → %s  %s\n", elide(r, 16), spark(vals), num(first), num(last), delta(first, last))
		if len(pts) > 1 {
			fmt.Printf("%-16s %s → %s  (%d points)\n", "",
				pts[0].t.Format("2006-01-02"), pts[len(pts)-1].t.Format("2006-01-02"), len(pts))
		}
	}
	return nil
}

type pt struct {
	t time.Time
	v float64
}

// bucketLast takes the final reading in each period — the state the codebase
// was left in, which is what a trend should show. A mean over a period during
// which the value changed is a number that never existed.
func bucketLast(ms []schema.Measure, period string) []pt {
	key := func(t time.Time) time.Time {
		u := t.UTC()
		d := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		switch period {
		case "day":
			return d
		case "month":
			return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		default:
			return d.AddDate(0, 0, -(int(d.Weekday()+6) % 7))
		}
	}
	acc := map[time.Time]pt{}
	for _, m := range ms {
		k := key(m.TS)
		if cur, ok := acc[k]; !ok || m.TS.After(cur.t) {
			acc[k] = pt{m.TS, m.Value}
		}
	}
	out := make([]pt, 0, len(acc))
	for k, v := range acc {
		out = append(out, pt{k, v.v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].t.Before(out[j].t) })
	return out
}

// ---------- diff ----------

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	dir, repo, metric, scope := inputFlags(fs)
	since := fs.String("since", "", "YYYY-MM-DD (required)")
	n := fs.Int("n", 20, "how many movers to show")
	fs.Parse(args)
	if *since == "" {
		return fmt.Errorf("--since YYYY-MM-DD is required")
	}
	cut, err := time.Parse("2006-01-02", *since)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	if *scope == "" {
		*scope = "function"
	}
	if *metric == "" {
		*metric = "cognitive"
	}

	ms, err := read(*dir, store.Query{Repo: *repo, Metric: *metric, Scope: schema.Scope(*scope)})
	if err != nil {
		return err
	}
	ms = filter(ms, *repo, *metric, *scope)

	// before = last reading at or before the cut; after = last reading overall
	before, after := map[string]schema.Measure{}, map[string]schema.Measure{}
	for _, m := range ms {
		k := m.Repo + "\x00" + m.Path
		if !m.TS.After(cut) {
			if c, ok := before[k]; !ok || m.TS.After(c.TS) {
				before[k] = m
			}
		}
		if c, ok := after[k]; !ok || m.TS.After(c.TS) {
			after[k] = m
		}
	}

	type mover struct {
		path      string
		from, to  float64
		isNew     bool
		isRemoved bool
	}
	var movers []mover
	for k, a := range after {
		if b, ok := before[k]; ok {
			if a.Value != b.Value {
				movers = append(movers, mover{k, b.Value, a.Value, false, false})
			}
		} else {
			movers = append(movers, mover{k, 0, a.Value, true, false})
		}
	}
	for k, b := range before {
		if _, ok := after[k]; !ok {
			movers = append(movers, mover{k, b.Value, 0, false, true})
		}
	}
	if len(movers) == 0 {
		fmt.Printf("no change in %s since %s\n", *metric, *since)
		return nil
	}
	sort.Slice(movers, func(i, j int) bool {
		return math.Abs(movers[i].to-movers[i].from) > math.Abs(movers[j].to-movers[j].from)
	})

	worse, better := 0, 0
	for _, m := range movers {
		if m.to > m.from {
			worse++
		} else {
			better++
		}
	}
	fmt.Printf("%s since %s — %d worse, %d better\n\n", *metric, *since, worse, better)
	if len(movers) > *n {
		movers = movers[:*n]
	}
	for _, m := range movers {
		label := strings.Replace(m.path, "\x00", " ", 1)
		tag := ""
		switch {
		case m.isNew:
			tag = " (new)"
		case m.isRemoved:
			tag = " (gone)"
		}
		fmt.Printf("  %6s → %-6s %-8s %s%s\n", num(m.from), num(m.to), delta(m.from, m.to), elide(label, 60), tag)
	}
	return nil
}

// ---------- compare ----------

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	dir, repo, metric, scope := inputFlags(fs)
	fs.Parse(args)
	if *scope == "" {
		*scope = "project"
	}

	ms, err := read(*dir, store.Query{Repo: *repo, Metric: *metric, Scope: schema.Scope(*scope)})
	if err != nil {
		return err
	}
	ms = filter(ms, *repo, *metric, *scope)
	if len(ms) == 0 {
		return fmt.Errorf("no measures at scope %s", *scope)
	}

	// latest value per repo+metric
	latest := map[string]map[string]float64{}
	for _, m := range ms {
		if latest[m.Metric] == nil {
			latest[m.Metric] = map[string]float64{}
		}
		latest[m.Metric][m.Repo] = m.Value
	}
	repos := map[string]bool{}
	for _, r := range latest {
		for k := range r {
			repos[k] = true
		}
	}
	rs := make([]string, 0, len(repos))
	for r := range repos {
		rs = append(rs, r)
	}
	sort.Strings(rs)

	fmt.Printf("%-24s", "metric")
	for _, r := range rs {
		fmt.Printf("%14s", elide(r, 13))
	}
	fmt.Println()
	fmt.Println(strings.Repeat("─", 24+14*len(rs)))
	for _, mt := range sortedFloatKeys(latest) {
		fmt.Printf("%-24s", elide(mt, 23))
		for _, r := range rs {
			if v, ok := latest[mt][r]; ok {
				fmt.Printf("%14s", num(v))
			} else {
				fmt.Printf("%14s", "–")
			}
		}
		fmt.Println()
	}
	return nil
}

// ---------- presentation ----------

var blocks = []rune("▁▂▃▄▅▆▇█")

// spark renders a sparkline. Scaled to the series' own min/max, because the
// question a trend answers is "did this move", not "is this big".
func spark(v []float64) string {
	if len(v) == 0 {
		return ""
	}
	lo, hi := v[0], v[0]
	for _, x := range v {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	var b strings.Builder
	for _, x := range v {
		if hi == lo {
			b.WriteRune(blocks[len(blocks)/2]) // flat series: mid-height, not empty
			continue
		}
		i := int((x - lo) / (hi - lo) * float64(len(blocks)-1))
		b.WriteRune(blocks[i])
	}
	return b.String()
}

func bar(v, max float64, width int) string {
	if max <= 0 {
		return strings.Repeat(" ", width)
	}
	n := int(v / max * float64(width))
	return strings.Repeat("█", n) + strings.Repeat("·", width-n)
}

// delta shows direction. Higher is worse for every metric assay emits, so up
// is flagged and down is not.
func delta(from, to float64) string {
	switch {
	case to > from:
		if from == 0 {
			return "▲ new"
		}
		return fmt.Sprintf("▲ +%.0f%%", (to-from)/math.Abs(from)*100)
	case to < from:
		if to == 0 {
			return "▼ gone"
		}
		return fmt.Sprintf("▼ %.0f%%", (to-from)/math.Abs(from)*100)
	}
	return "  flat"
}

// num formats a value for a human: integers stay integers, and a float gets two
// decimals rather than the seventeen that %g is happy to print.
func num(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e9 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func elide(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return "…" + s[len(s)-n+1:] // keep the tail: filenames matter more than paths
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedFloatKeys(m map[string]map[string]float64) []string {
	return sortedKeys(m)
}
