// Command ratchet measures Go code quality and holds a line against regression.
//
//	ratchet scan     ./...        measure, and report findings
//	ratchet baseline ./...        record today's state as tolerated
//	ratchet check    ./...        fail only if something got worse
//	ratchet history  ./...        emit a metric series over git history
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sherzing/ratchet/internal/analyze"
	"github.com/sherzing/ratchet/internal/baseline"
	"github.com/sherzing/ratchet/internal/model"
	"github.com/sherzing/ratchet/internal/report"
)

const version = "0.1.0"

const defaultBaselineFile = ".ratchet-baseline.json"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "scan":
		err = cmdScan(os.Args[2:])
	case "baseline":
		err = cmdBaseline(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "history":
		err = cmdHistory(os.Args[2:])
	case "rules":
		cmdRules()
	case "version", "-v", "--version":
		fmt.Println("ratchet", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "ratchet: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ratchet:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ratchet — measure Go code quality, and hold the line against regression

usage:
  ratchet scan     [flags] [path]   measure and report
  ratchet baseline [flags] [path]   record current state as tolerated
  ratchet check    [flags] [path]   exit non-zero only on regression
  ratchet history  [flags] [path]   metric series over git history
  ratchet rules                     list rules
  ratchet version

run "ratchet <command> -h" for flags
`)
}

// commonFlags are shared by the scanning commands.
type commonFlags struct {
	format        string
	rules         string
	skipDirs      string
	includeTests  bool
	includeVendor bool
	detail        bool
	top           int
}

func bindCommon(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.format, "format", "text", "output: text|json|csv|sarif")
	fs.StringVar(&c.rules, "rules", "", "comma-separated rule IDs to enable (default: all)")
	fs.StringVar(&c.skipDirs, "skip-dirs", "", "comma-separated extra directory names to skip")
	fs.BoolVar(&c.includeTests, "include-tests", false, "analyse _test.go files")
	fs.BoolVar(&c.includeVendor, "include-vendor", false, "analyse vendor/")
	fs.BoolVar(&c.detail, "detail", true, "include per-function records")
	fs.IntVar(&c.top, "top", 10, "hotspots to list in text output")
}

func (c commonFlags) options() analyze.Options {
	opt := analyze.Options{
		IncludeTests:  c.includeTests,
		IncludeVendor: c.includeVendor,
		Detail:        c.detail,
	}
	if c.rules != "" {
		opt.Enabled = map[string]bool{}
		for _, r := range strings.Split(c.rules, ",") {
			opt.Enabled[strings.TrimSpace(r)] = true
		}
	}
	for _, d := range strings.Split(c.skipDirs, ",") {
		if d = strings.TrimSpace(d); d != "" {
			opt.SkipDirs = append(opt.SkipDirs, d)
		}
	}
	return opt
}

// parseArgs parses flags that appear before OR after the path argument.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `ratchet scan . --format json` silently ignores --format and emits text.
// Everyone types the path first, so accepting only the other order is a trap —
// and one this tool's own README fell into. We loop: parse, pull off the
// positional the parser stopped on, parse the remainder, repeat.
func parseArgs(fs *flag.FlagSet, args []string) string {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return "."
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	if len(positional) == 0 {
		return "."
	}
	// Accept the ./... idiom people type by reflex, but we walk the tree
	// ourselves rather than resolving package patterns.
	return strings.TrimSuffix(strings.TrimSuffix(positional[0], "..."), "/")
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	var c commonFlags
	bindCommon(fs, &c)
	failOn := fs.String("fail-on", "", "exit non-zero if any finding at or above this severity: error|warn|info")
	root := parseArgs(fs, args)
	rep, err := analyze.Scan(root, c.options())
	if err != nil {
		return err
	}
	if err := emit(rep, c); err != nil {
		return err
	}
	if *failOn != "" && exceeds(rep, model.Severity(*failOn)) {
		os.Exit(1)
	}
	return nil
}

func exceeds(rep *model.Report, min model.Severity) bool {
	rank := map[model.Severity]int{model.Info: 1, model.Warn: 2, model.Error: 3}
	for _, f := range rep.Findings {
		if rank[f.Severity] >= rank[min] {
			return true
		}
	}
	return false
}

func emit(rep *model.Report, c commonFlags) error {
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	switch c.format {
	case "json":
		return report.JSON(out, rep)
	case "csv":
		return report.CSV(out, rep)
	case "sarif":
		docs := map[string]string{}
		for id, r := range analyze.Rules {
			docs[id] = r.Doc
		}
		return report.SARIF(out, rep, docs)
	case "text":
		if err := report.Text(out, rep, c.top); err != nil {
			return err
		}
		report.Findings(out, rep.Findings)
		return nil
	}
	return fmt.Errorf("unknown format %q", c.format)
}

func cmdBaseline(args []string) error {
	fs := flag.NewFlagSet("baseline", flag.ExitOnError)
	var c commonFlags
	bindCommon(fs, &c)
	file := fs.String("file", defaultBaselineFile, "baseline path")
	force := fs.Bool("force", false, "overwrite an existing baseline")
	tighten := fs.Bool("tighten", false, "drop entries that no longer reproduce, keep the rest")
	root := parseArgs(fs, args)
	rep, err := analyze.Scan(root, c.options())
	if err != nil {
		return err
	}
	path := *file
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}

	if _, err := os.Stat(path); err == nil && !*force && !*tighten {
		// The whole point of a ratchet is that you cannot quietly reset it.
		// Requiring an explicit flag means a rewrite shows up in review.
		return fmt.Errorf("baseline already exists at %s\n"+
			"  regenerating it would silently forgive every current violation.\n"+
			"  use --tighten to drop only what is genuinely fixed, or --force to overwrite", path)
	}

	commit := gitCommit(root)
	if *tighten {
		b, err := baseline.Load(path)
		if err != nil {
			return err
		}
		n := b.Tighten(rep)
		b.Commit = commit
		if err := b.Save(path); err != nil {
			return err
		}
		fmt.Printf("tightened %s: removed %d fixed entries, %d remain\n", path, n, len(b.Tolerated))
		return nil
	}

	b := baseline.From(rep, commit)
	if err := b.Save(path); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n  %d tolerated findings\n  caps: cyclomatic<=%d cognitive<=%d nesting<=%d\n",
		path, len(b.Tolerated), b.Caps.MaxCyclomatic, b.Caps.MaxCognitive, b.Caps.MaxNesting)
	fmt.Println("\ncommit this file — it is the record of what we agreed to tolerate")
	return nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var c commonFlags
	bindCommon(fs, &c)
	file := fs.String("file", defaultBaselineFile, "baseline path")
	strictCaps := fs.Bool("strict-caps", false, "also fail if max complexity exceeds the baseline")
	asJSON := fs.Bool("json", false, "emit the result as JSON")
	root := parseArgs(fs, args)
	path := *file
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	b, err := baseline.Load(path)
	if err != nil {
		return fmt.Errorf("%w\n  run `ratchet baseline` first", err)
	}
	rep, err := analyze.Scan(root, c.options())
	if err != nil {
		return err
	}
	res := b.Check(rep, *strictCaps)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		fmt.Printf("%d tolerated, %d new, %d fixed\n", res.Existing, len(res.New), len(res.Fixed))
		if len(res.Fixed) > 0 {
			fmt.Printf("\n%d findings no longer present — run `ratchet baseline --tighten` to lock that in\n", len(res.Fixed))
		}
		if len(res.New) > 0 {
			fmt.Printf("\nNEW findings (these fail the build):\n")
			report.Findings(os.Stdout, res.New)
		}
		for _, cb := range res.CapBreak {
			fmt.Printf("\ncap breach: %s\n", cb)
		}
	}
	if res.Regressed() {
		os.Exit(1)
	}
	return nil
}

// cmdHistory replays the metric summary over git history.
//
// This one genuinely needs a checkout per sample: per-function complexity
// requires a parsed tree, which a diff cannot give us. Checkout dominates the
// cost (~0.7s vs ~15ms for diff-only work), so we sample rather than replay
// every commit, and we use `git archive` into a temp dir instead of mutating the
// working tree — the latter would be unusable on a machine someone is working on.
func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	var c commonFlags
	bindCommon(fs, &c)
	since := fs.String("since", "", "git --since value (empty = from the first commit)")
	interval := fs.String("interval", "1 week", "sampling interval: all|1 day|1 week|1 month")
	maxN := fs.Int("max", 0, "maximum samples (0 = unlimited)")
	firstParent := fs.Bool("first-parent", true,
		"follow only the merge timeline. Feature-branch intermediate commits are work in progress, not states the codebase was ever really in")
	jobs := fs.Int("jobs", 4, "parallel workers. Checkout dominates the cost, so this scales well")
	root := parseArgs(fs, args)
	c.detail = false // only the summary is kept for a series

	commits, err := sampleCommits(root, *since, *interval, *maxN, *firstParent)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return fmt.Errorf("no commits found in range")
	}
	fmt.Fprintf(os.Stderr, "sampling %d commits with %d workers\n", len(commits), *jobs)

	// Each worker gets its own temp checkout. Results are collected then
	// emitted in commit order, because a series that arrives out of order is
	// useless for a trend and callers should not have to sort it.
	type slot struct {
		rep *model.Report
		err error
	}
	out := make([]slot, len(commits))
	opts := c.options()

	sem := make(chan struct{}, max(1, *jobs))
	var wg sync.WaitGroup
	var done int64

	for i, sha := range commits {
		wg.Add(1)
		go func(i int, sha string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			dir, err := os.MkdirTemp("", "ratchet-*")
			if err != nil {
				out[i] = slot{err: err}
				return
			}
			defer os.RemoveAll(dir)

			if err := gitArchive(root, sha, dir); err != nil {
				out[i] = slot{err: err}
				return
			}
			rep, err := analyze.Scan(dir, opts)
			if err != nil {
				out[i] = slot{err: err}
				return
			}
			rep.Commit = sha
			rep.Root = ""
			out[i] = slot{rep: rep}

			if n := atomic.AddInt64(&done, 1); n%25 == 0 || int(n) == len(commits) {
				fmt.Fprintf(os.Stderr, "  %d/%d\n", n, len(commits))
			}
		}(i, sha)
	}
	wg.Wait()

	enc := json.NewEncoder(os.Stdout)
	skipped := 0
	for i, s := range out {
		if s.rep == nil {
			skipped++
			fmt.Fprintf(os.Stderr, "  skipped %s: %v\n", commits[i][:8], s.err)
			continue
		}
		if err := enc.Encode(s.rep); err != nil {
			return err
		}
	}
	// Silent truncation reads as "covered everything" when it did not.
	fmt.Fprintf(os.Stderr, "emitted %d records, skipped %d\n", len(commits)-skipped, skipped)
	return nil
}

// sampleCommits picks commits to measure. interval "all" takes every commit;
// otherwise one per bucket. git's own --since/--until bucketing is awkward here,
// so we take the log and thin it by date ourselves.
func sampleCommits(root, since, interval string, maxN int, firstParent bool) ([]string, error) {
	logArgs := []string{"log"}
	if firstParent {
		logArgs = append(logArgs, "--first-parent")
	}
	if since != "" {
		logArgs = append(logArgs, "--since="+since)
	}
	logArgs = append(logArgs, "--date=format:%Y-%m-%d", "--pretty=format:%H %ad")
	out, err := gitOut(root, logArgs...)
	if err != nil {
		return nil, err
	}
	bucket := func(date string, idx int) string {
		switch interval {
		case "all":
			return fmt.Sprint(idx) // unique per commit: nothing is thinned
		case "1 day":
			return date
		case "1 month":
			return date[:7]
		default: // 1 week — ISO-ish bucketing by day-of-month block
			return date[:7] + "-w" + fmt.Sprint((atoi(date[8:10])-1)/7)
		}
	}
	seen := map[string]bool{}
	var picked []string
	for idx, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		b := bucket(parts[1], idx)
		if seen[b] {
			continue
		}
		seen[b] = true
		picked = append(picked, parts[0])
		if maxN > 0 && len(picked) >= maxN {
			break
		}
	}
	// Oldest first, so the emitted series reads forward in time.
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked, nil
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func gitArchive(root, sha, dest string) error {
	cmd := exec.Command("git", "-C", root, "archive", sha)
	tar := exec.Command("tar", "-x", "-C", dest)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	tar.Stdin = pipe
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := tar.Run(); err != nil {
		return err
	}
	return cmd.Wait()
}

func gitOut(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	b, err := cmd.Output()
	return string(b), err
}

func gitCommit(root string) string {
	out, err := gitOut(root, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func cmdRules() {
	ids := make([]string, 0, len(analyze.Rules))
	for id := range analyze.Rules {
		ids = append(ids, id)
	}
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	for _, id := range ids {
		r := analyze.Rules[id]
		fmt.Printf("%-28s [%s]\n  %s\n\n", id, r.Severity, r.Doc)
	}
}
