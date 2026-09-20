package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sherzing/assay/pkg/schema"
)

// ---- helpers -------------------------------------------------------------

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), ".assay"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func measure(repo, metric string, at time.Time, v float64) *schema.Measure {
	return &schema.Measure{Repo: repo, Metric: metric, TS: at, Scope: schema.ScopeProject, Value: v}
}

func finding(repo, rule, fp string, at time.Time) *schema.Finding {
	return &schema.Finding{
		Repo: repo, Rule: rule, Fingerprint: fp, TS: at,
		Severity: schema.SevWarn, File: "a.go", Message: "m",
	}
}

func verdictRec(fp, rule, org string, j schema.Judgement, at time.Time) *schema.Verdict {
	return &schema.Verdict{Fingerprint: fp, Rule: rule, Org: org, Verdict: j, TS: at}
}

// stream encodes records the way any producer on the pipe would, so the tests
// exercise the real decode path rather than reaching past it.
func stream(t *testing.T, recs ...any) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	enc := schema.NewEncoder(&b)
	for _, r := range recs {
		if err := enc.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func appendAll(t *testing.T, s *Store, recs ...any) (int, int, int) {
	t.Helper()
	m, f, v, err := s.Append(stream(t, recs...))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return m, f, v
}

// sameMeasure compares by value, with TS compared by instant rather than by
// representation — a wall clock that survives JSON is the same instant even if
// its Location pointer is not the same object.
func sameMeasure(a, b schema.Measure) bool {
	if !a.TS.Equal(b.TS) {
		return false
	}
	a.TS, b.TS = time.Time{}, time.Time{}
	return a == b
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n")
}

// writeRaw drops arbitrary bytes into a partition, which is how a truncated
// process or a half-synced file would leave it.
func writeRaw(t *testing.T, s *Store, kind, ymd, content string) string {
	t.Helper()
	p := filepath.Join(append([]string{s.Root, kind}, strings.Split(ymd, "/")...)...) + ".jsonl"
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- round trip ----------------------------------------------------------

// The data layer's one promise: what went in comes back out unchanged. Unicode
// and embedded newlines are the two payloads that break a line-delimited format
// if anything anywhere does its own string handling — a raw newline inside a
// value would split one record into two unparseable ones.
func TestRoundTripPreservesRecordsExactly(t *testing.T) {
	s := openStore(t)
	at := ts("2026-09-19T10:00:00.123456789Z")

	awkward := []string{
		"café/naïve/日本語/🔬/Ω",
		"line one\nline two\nline three",
		"tab\there and carriage\rreturn",
		`quoted "value" and backslash C:\repo\pkg`,
		"<script>alert(1)&</script>",
		"trailing space and null-ish \\u0000 literal ",
	}

	var recs []any
	var want []schema.Measure
	for i, a := range awkward {
		m := measure("repo/"+a, "metric."+a, at.Add(time.Duration(i)*time.Second), float64(i)+0.5)
		m.Path = "src/" + a
		m.Commit = "c0ffee" + a
		recs = append(recs, m)
	}
	appendAll(t, s, recs...)
	for _, r := range recs {
		want = append(want, *r.(*schema.Measure))
	}

	got, err := s.QueryMeasures(Query{})
	if err != nil {
		t.Fatalf("QueryMeasures: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d measures, want %d", len(got), len(want))
	}
	for i := range want {
		if !sameMeasure(got[i], want[i]) {
			t.Errorf("record %d round-tripped as\n %#v\nwant\n %#v", i, got[i], want[i])
		}
	}

	// One record per line, no matter what the strings contain. If an embedded
	// newline ever escapes, this is the assertion that catches it.
	body := readFile(t, filepath.Join(s.Root, "measures", "2026", "09", "19.jsonl"))
	if n := countLines(body); n != len(want) {
		t.Errorf("partition holds %d lines for %d records — a value broke the line framing", n, len(want))
	}
}

// Findings and verdicts carry free text too, and they take different code paths
// through Append than measures do.
func TestRoundTripFindingsAndVerdicts(t *testing.T) {
	s := openStore(t)
	at := ts("2026-09-19T10:00:00Z")

	f := finding("billing-api", "no-context-todo", "9f2a3c4d", at)
	f.Message = "TODO: 見直す\nowner: sven\n"
	f.Suggest = "use ctx\twith timeout"
	v := verdictRec("9f2a3c4d", "no-context-todo", "acme", schema.Accepted, at)
	v.Reason = "PRO-4412\nscheduled for Q4 — “quoted”"

	m, fn, vn := appendAll(t, s, f, v)
	if m != 0 || fn != 1 || vn != 1 {
		t.Fatalf("Append counted (m=%d f=%d v=%d), want (0,1,1)", m, fn, vn)
	}

	vs, err := s.Verdicts()
	if err != nil {
		t.Fatal(err)
	}
	if got := vs["9f2a3c4d"]; got.Reason != v.Reason || got.Verdict != schema.Accepted || !got.TS.Equal(at) {
		t.Errorf("verdict round-tripped as %#v, want reason %q", got, v.Reason)
	}

	seen, _, err := s.findingHistory()
	if err != nil {
		t.Fatal(err)
	}
	if info, ok := seen["9f2a3c4d"]; !ok || info.rule != "no-context-todo" || info.repo != "billing-api" {
		t.Errorf("finding round-tripped as %#v", info)
	}

	// Two records, two lines, despite the newlines inside both of them.
	if n := countLines(readFile(t, filepath.Join(s.Root, "findings", "2026", "09", "19.jsonl"))); n != 1 {
		t.Errorf("findings partition has %d lines, want 1", n)
	}
	if n := countLines(readFile(t, filepath.Join(s.Root, "verdicts", "verdicts.jsonl"))); n != 1 {
		t.Errorf("verdict log has %d lines, want 1", n)
	}
}

// ---- durability ----------------------------------------------------------

// THE LOAD-BEARING TEST. Append must open with O_APPEND, never O_TRUNC. If this
// ever regresses, every run silently discards the history that came before it
// and nobody notices until a trend is asked for months later.
func TestAppendIsAdditiveNeverDestructive(t *testing.T) {
	s := openStore(t)
	day := ts("2026-09-19T08:00:00Z")

	var want []schema.Measure
	for i := 0; i < 5; i++ {
		// Five separate Append calls, i.e. five separate process runs.
		m := measure("billing-api", "coverage", day.Add(time.Duration(i)*time.Hour), float64(i))
		appendAll(t, s, m)
		want = append(want, *m)

		got, err := s.QueryMeasures(Query{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != i+1 {
			t.Fatalf("after %d appends the store holds %d records, want %d — an append truncated the partition",
				i+1, len(got), i+1)
		}
	}

	got, err := s.QueryMeasures(Query{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if !sameMeasure(got[i], want[i]) {
			t.Errorf("record %d = %#v, want %#v", i, got[i], want[i])
		}
	}

	// Re-opening the store must not touch existing data either.
	if _, err := Open(s.Root); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.QueryMeasures(Query{}); len(got) != len(want) {
		t.Errorf("Open() on an existing store left %d records, want %d", len(got), len(want))
	}
}

func TestAppendTicketIsAdditive(t *testing.T) {
	s := openStore(t)
	at := ts("2026-09-19T10:00:00Z")
	for i := 0; i < 3; i++ {
		tk := schema.Ticket{
			Provider: "jira", ID: fmt.Sprintf("PRO-%d", i), GroupBy: "rule",
			Group: "no-context-todo", Title: "clean up", State: "open",
			Fingerprints: []string{"a", "b"}, TS: at.Add(time.Duration(i) * time.Minute),
		}
		if err := s.AppendTicket(tk); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Tickets()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ticket log holds %d events, want 3 — history was overwritten", len(got))
	}
	if !reflect.DeepEqual(got[0].Fingerprints, []string{"a", "b"}) {
		t.Errorf("fingerprints = %v, want [a b]", got[0].Fingerprints)
	}
	for i, want := range []string{"PRO-0", "PRO-1", "PRO-2"} {
		if got[i].ID != want {
			t.Errorf("event %d is %s, want %s — the log is not in append order", i, got[i].ID, want)
		}
	}
}

// ---- partitioning --------------------------------------------------------

func TestRecordsLandInDatePartitions(t *testing.T) {
	s := openStore(t)
	cases := []struct {
		at   string
		file string
	}{
		{"2026-09-19T10:00:00Z", "2026/09/19.jsonl"},
		{"2026-09-20T00:30:00Z", "2026/09/20.jsonl"},
		// Partitioning is by UTC day, not by the offset the producer happened to
		// use — otherwise the same instant lands in two different files.
		{"2026-09-19T23:30:00-05:00", "2026/09/20.jsonl"},
		{"2026-01-02T00:00:00Z", "2026/01/02.jsonl"},
		{"2027-12-31T23:59:59Z", "2027/12/31.jsonl"},
	}
	for i, c := range cases {
		appendAll(t, s, measure("r", "m", ts(c.at), float64(i)))
	}
	for _, c := range cases {
		p := filepath.Join(s.Root, "measures", filepath.FromSlash(c.file))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s did not land in %s: %v", c.at, c.file, err)
		}
	}
	// The two September-20 records share a partition; nothing else does.
	if n := countLines(readFile(t, filepath.Join(s.Root, "measures", "2026", "09", "20.jsonl"))); n != 2 {
		t.Errorf("2026/09/20.jsonl holds %d records, want 2", n)
	}
}

// Verdicts are deliberately NOT date-partitioned: the whole log is read to
// resolve current state, so splitting it by day would only add directory walks.
func TestVerdictsAreASingleLog(t *testing.T) {
	s := openStore(t)
	appendAll(t, s,
		verdictRec("fp1", "r", "acme", schema.Accepted, ts("2026-01-05T10:00:00Z")),
		verdictRec("fp2", "r", "acme", schema.WontFix, ts("2026-09-19T10:00:00Z")),
	)
	if n := countLines(readFile(t, filepath.Join(s.Root, "verdicts", "verdicts.jsonl"))); n != 2 {
		t.Errorf("verdict log holds %d lines, want both verdicts in one file", n)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root, "verdicts")); len(entries) != 1 {
		t.Errorf("verdicts dir holds %d entries, want exactly verdicts.jsonl", len(entries))
	}
}

func TestQuerySpansPartitionsInTimeOrder(t *testing.T) {
	s := openStore(t)
	// Appended out of order and across three partitions, including a year edge.
	times := []string{
		"2026-09-20T09:00:00Z",
		"2026-09-19T09:00:00Z",
		"2027-01-01T09:00:00Z",
		"2026-09-19T21:00:00Z",
		"2026-12-31T09:00:00Z",
	}
	for i, at := range times {
		appendAll(t, s, measure("r", "m", ts(at), float64(i)))
	}

	got, err := s.QueryMeasures(Query{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2026-09-19T09:00:00Z", "2026-09-19T21:00:00Z", "2026-09-20T09:00:00Z",
		"2026-12-31T09:00:00Z", "2027-01-01T09:00:00Z",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records across partitions, want %d", len(got), len(want))
	}
	for i, w := range want {
		if !got[i].TS.Equal(ts(w)) {
			t.Errorf("position %d is %s, want %s — results are not in time order", i, got[i].TS.Format(time.RFC3339), w)
		}
	}
}

func TestQueryDateRangeIsInclusiveAndSkipsOtherPartitions(t *testing.T) {
	s := openStore(t)
	for _, at := range []string{
		"2026-09-18T23:59:59Z",
		"2026-09-19T00:00:00Z",
		"2026-09-19T12:00:00Z",
		"2026-09-20T12:00:00Z",
		"2026-09-21T00:00:01Z",
	} {
		appendAll(t, s, measure("r", "m", ts(at), 1))
	}
	got, err := s.QueryMeasures(Query{
		Since: ts("2026-09-19T00:00:00Z"),
		Until: ts("2026-09-21T00:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("range query returned %d records, want 3 (both bounds inclusive)", len(got))
	}
	for _, m := range got {
		if m.TS.Before(ts("2026-09-19T00:00:00Z")) || m.TS.After(ts("2026-09-21T00:00:00Z")) {
			t.Errorf("record at %s is outside the requested range", m.TS.Format(time.RFC3339))
		}
	}
}

// Partitions are day-granular but the filter is not: two measurements on the
// same day must be separable by time of day, which means the record-level
// filter has to run as well as the directory-level one.
func TestDateFilterAppliesWithinAPartitionAndToTheWalk(t *testing.T) {
	s := openStore(t)
	for _, at := range []string{
		"2026-09-19T01:00:00Z", "2026-09-19T13:00:00Z", "2026-09-19T23:00:00Z",
	} {
		appendAll(t, s, measure("r", "m", ts(at), 1))
	}
	appendAll(t, s, measure("r", "m", ts("2026-09-21T01:00:00Z"), 1)) // a later partition

	got, err := s.QueryMeasures(Query{
		Since: ts("2026-09-19T12:00:00Z"),
		Until: ts("2026-09-20T12:00:00Z"), // excludes the 09-21 partition at the walk
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want the 2 from the afternoon of the 19th: %+v", len(got), got)
	}
	if !got[0].TS.Equal(ts("2026-09-19T13:00:00Z")) {
		t.Errorf("first record is %s, want 13:00 — the morning record was not filtered out",
			got[0].TS.Format(time.RFC3339))
	}
}

func TestQueryFilters(t *testing.T) {
	s := openStore(t)
	at := ts("2026-09-19T10:00:00Z")
	a := measure("billing-api", "coverage", at, 1)
	a.Path = "internal/pay/card.go"
	a.Scope = schema.ScopeFile
	b := measure("billing-api", "complexity", at.Add(time.Minute), 2)
	b.Path = "internal/pay/bank.go"
	b.Scope = schema.ScopeFile
	c := measure("orders-worker", "coverage", at.Add(2*time.Minute), 3)
	appendAll(t, s, a, b, c)

	cases := []struct {
		name string
		q    Query
		want int
	}{
		{"repo", Query{Repo: "billing-api"}, 2},
		{"metric", Query{Metric: "coverage"}, 2},
		{"scope", Query{Scope: schema.ScopeFile}, 2},
		{"path prefix", Query{Path: "internal/pay/"}, 2},
		{"exact path", Query{Path: "internal/pay/card.go"}, 1},
		{"repo and metric", Query{Repo: "billing-api", Metric: "coverage"}, 1},
		{"no match", Query{Repo: "nope"}, 0},
	}
	for _, c := range cases {
		got, err := s.QueryMeasures(c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: got %d records, want %d", c.name, len(got), c.want)
		}
	}
}

// ---- damaged and missing data -------------------------------------------

// CONTRACT: malformed lines are SKIPPED, not fatal. schema.Decode reports them
// through onErr (which the store passes as nil) and keeps reading, so one
// truncated write cannot cost you the rest of the partition. Query returns a
// nil error even though data was dropped.
func TestMalformedLinesAreSkippedAndNeighboursSurvive(t *testing.T) {
	s := openStore(t)
	at := ts("2026-09-19T10:00:00Z")
	good := func(v int) string {
		b, _ := json.Marshal(schema.Measure{
			V: 1, Kind: schema.KindMeasure, Repo: "r", TS: at.Add(time.Duration(v) * time.Minute),
			Scope: schema.ScopeProject, Metric: "m", Value: float64(v),
		})
		return string(b)
	}
	body := strings.Join([]string{
		good(1),
		`{"kind":"measure","v":1,"repo":"r","ts":`, // truncated mid-record
		good(2),
		`not json at all`,
		good(3),
		`{"kind":"measure","v":1,"ts":12345}`, // right kind, wrong field type
		good(4),
		``,                        // blank line
		`# a comment from a tool`, // comment line
		good(5),
	}, "\n") + "\n" + `{"kind":"measure","v":1,"repo":"r","ts"` // torn final line, no newline
	writeRaw(t, s, "measures", "2026/09/19", body)

	got, err := s.QueryMeasures(Query{})
	if err != nil {
		t.Fatalf("a corrupt line made the query fail; the contract is skip-and-continue: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d records, want the 5 valid ones either side of the damage: %+v", len(got), got)
	}
	for i, m := range got {
		if m.Value != float64(i+1) {
			t.Errorf("position %d is value %v, want %d — a valid record was lost", i, m.Value, i+1)
		}
	}
}

// A corrupt partition must not poison neighbouring days either.
func TestCorruptPartitionDoesNotBlockOtherPartitions(t *testing.T) {
	s := openStore(t)
	appendAll(t, s, measure("r", "m", ts("2026-09-18T10:00:00Z"), 1))
	appendAll(t, s, measure("r", "m", ts("2026-09-20T10:00:00Z"), 3))
	writeRaw(t, s, "measures", "2026/09/19", "\x00\x01 garbage \xff\xfe\nmore garbage\n")

	got, err := s.QueryMeasures(Query{})
	if err != nil {
		t.Fatalf("QueryMeasures: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d records, want the 2 healthy partitions to survive a corrupt neighbour", len(got))
	}
}

// A line too long for the decoder's 8 MiB buffer is NOT skipped the way a
// malformed line is — it kills the scan and the whole query errors, partial
// results and all. Worth knowing: the skip-and-continue contract has this one
// hole, and it takes out every partition, not just the damaged one.
func TestAnOversizedLineFailsTheQueryRatherThanBeingSkipped(t *testing.T) {
	s := openStore(t)
	appendAll(t, s, measure("r", "m", ts("2026-09-18T10:00:00Z"), 1))
	writeRaw(t, s, "measures", "2026/09/19", strings.Repeat("x", 9<<20)+"\n")

	if _, err := s.QueryMeasures(Query{}); err == nil {
		t.Error("an oversized line was tolerated; current behaviour is to fail the query")
	}
	if _, err := s.Rollup(Query{}, "day"); err == nil {
		t.Error("Rollup swallowed the query error")
	}
}

// A store that has never been written to is the first-run case, and first run
// must not look like an error.
func TestEmptyStoreReturnsEmptyNotError(t *testing.T) {
	never := &Store{Root: filepath.Join(t.TempDir(), "never-created")}
	opened := openStore(t)

	for name, s := range map[string]*Store{"nonexistent root": never, "freshly opened": opened} {
		if got, err := s.QueryMeasures(Query{}); err != nil || len(got) != 0 {
			t.Errorf("%s: QueryMeasures = (%d, %v), want (0, nil)", name, len(got), err)
		}
		if got, err := s.Verdicts(); err != nil || len(got) != 0 {
			t.Errorf("%s: Verdicts = (%d, %v), want (0, nil)", name, len(got), err)
		}
		if got, err := s.Tickets(); err != nil || len(got) != 0 {
			t.Errorf("%s: Tickets = (%d, %v), want (0, nil)", name, len(got), err)
		}
		if got, err := s.Precision(); err != nil || len(got) != 0 {
			t.Errorf("%s: Precision = (%d, %v), want (0, nil)", name, len(got), err)
		}
		if got, err := s.Rollup(Query{}, "week"); err != nil || len(got) != 0 {
			t.Errorf("%s: Rollup = (%d, %v), want (0, nil)", name, len(got), err)
		}
	}
}

func TestOpenDefaultsToDotAssayAndCreatesLayout(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"measures", "findings", "verdicts", "tickets", "rollup"} {
		info, err := os.Stat(filepath.Join(root, d))
		if err != nil || !info.IsDir() {
			t.Errorf("Open did not create %s/: %v", d, err)
		}
	}
	if s.Root != root {
		t.Errorf("Root = %q, want %q", s.Root, root)
	}

	// An empty root means ".assay" relative to the working directory. Run it in
	// a temp dir so the repo is not polluted.
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if d.Root != ".assay" {
		t.Errorf("Open(\"\").Root = %q, want .assay", d.Root)
	}
}

// SUSPECT: store.go:69-76 — Append's deferred cleanup discards the error from
// both Flush and Close (`_ = e.Flush()`, `_ = h.Close()`). A write that fails at
// flush time (ENOSPC, EIO, a full container volume) loses every buffered record
// and Append still returns a nil error with a non-zero count, so the caller
// reports "wrote 412 measures" having written none. Failures that happen before
// the first byte — as here, where the partition directory cannot be created —
// ARE reported, which is the behaviour asserted below.
func TestAppendReportsAnUnwritablePartition(t *testing.T) {
	s := openStore(t)
	// Put a regular file where the year directory needs to be.
	blocker := filepath.Join(s.Root, "measures", "2026")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := s.Append(stream(t, measure("r", "m", ts("2026-09-19T10:00:00Z"), 1)))
	if err == nil {
		t.Error("Append silently succeeded against an unwritable partition")
	}

	// The same for a partition path that exists but cannot be opened for writing.
	other := openStore(t)
	if err := os.MkdirAll(filepath.Join(other.Root, "measures", "2026", "09", "19.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := other.Append(stream(t, measure("r", "m", ts("2026-09-19T10:00:00Z"), 1))); err == nil {
		t.Error("Append silently succeeded writing to a directory")
	}
}

func TestStoreOperationsReportUnusableFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "not-a-store"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(root, "not-a-store")); err == nil {
		t.Error("Open accepted a regular file as a store root")
	}

	s := openStore(t)
	// A directory where the ticket log belongs: it opens but cannot be read,
	// and it cannot be written either.
	if err := os.MkdirAll(filepath.Join(s.Root, "tickets", "tickets.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tickets(); err == nil {
		t.Error("Tickets returned no error for an unreadable log")
	}
	if err := s.AppendTicket(schema.Ticket{Provider: "jira", ID: "PRO-1"}); err == nil {
		t.Error("AppendTicket returned no error writing to a directory")
	}

	// And a regular file where the tickets directory belongs.
	blocked := openStore(t)
	if err := os.RemoveAll(filepath.Join(blocked.Root, "tickets")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked.Root, "tickets"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := blocked.AppendTicket(schema.Ticket{Provider: "jira", ID: "PRO-1"}); err == nil {
		t.Error("AppendTicket returned no error when the directory could not be created")
	}
}

// Records with no timestamp are stamped on arrival rather than rejected, so a
// producer that forgets `ts` still lands in a sane partition.
func TestAppendStampsMissingTimestamps(t *testing.T) {
	s := openStore(t)
	before := time.Now().UTC().Add(-time.Second)
	appendAll(t, s,
		&schema.Measure{Repo: "r", Metric: "m", Scope: schema.ScopeProject, Value: 1},
		finding("r", "rule", "fp1", time.Time{}),
		verdictRec("fp1", "rule", "acme", schema.Accepted, time.Time{}),
	)
	got, err := s.QueryMeasures(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d measures, want 1", len(got))
	}
	if got[0].TS.Before(before) || got[0].TS.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("stamped TS = %s, want roughly now", got[0].TS)
	}
	vs, _ := s.Verdicts()
	if vs["fp1"].TS.IsZero() {
		t.Error("verdict kept a zero timestamp")
	}
}

// SUSPECT: store.go:120-122 — Append silently drops ticket records. schema
// defines KindTicket and Decode routes it, but Append's switch has no ticket
// case, so `cat tickets.jsonl | assay append` reports success and writes
// nothing. Tickets can only be stored through AppendTicket. Asserting the
// current behaviour so a future fix is a deliberate change.
func TestAppendDropsTicketRecords(t *testing.T) {
	s := openStore(t)
	tk := &schema.Ticket{
		Provider: "jira", ID: "PRO-1", GroupBy: "rule", Group: "g", Title: "t",
		State: "open", TS: ts("2026-09-19T10:00:00Z"),
	}
	m, f, v := appendAll(t, s, tk, measure("r", "m", ts("2026-09-19T10:00:00Z"), 1))
	if m != 1 || f != 0 || v != 0 {
		t.Errorf("counts = (%d,%d,%d), want (1,0,0)", m, f, v)
	}
	got, err := s.Tickets()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("Append stored %d tickets; current behaviour is to drop them", len(got))
	}
}

func TestVerdictsLastWriteWins(t *testing.T) {
	s := openStore(t)
	at := ts("2026-09-19T10:00:00Z")
	appendAll(t, s, verdictRec("fp1", "r", "acme", schema.Accepted, at))
	appendAll(t, s, verdictRec("fp1", "r", "acme", schema.FalsePositive, at.Add(time.Hour)))
	appendAll(t, s, verdictRec("fp2", "r", "acme", schema.WontFix, at.Add(2*time.Hour)))

	vs, err := s.Verdicts()
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 {
		t.Fatalf("resolved %d fingerprints, want 2", len(vs))
	}
	if vs["fp1"].Verdict != schema.FalsePositive {
		t.Errorf("fp1 = %q, want the later verdict false-positive", vs["fp1"].Verdict)
	}
	// The superseded line is still on disk: that is the audit trail.
	if n := countLines(readFile(t, filepath.Join(s.Root, "verdicts", "verdicts.jsonl"))); n != 3 {
		t.Errorf("verdict log holds %d lines, want all 3 including the superseded one", n)
	}
}

// ---- concurrency ---------------------------------------------------------

// Several processes (or goroutines) writing the same day's partition at once is
// the normal case on CI: many repos finish their scans together. O_APPEND makes
// each write land at the end of the file, so no writer can clobber another's
// bytes, and a record that fits in one flush arrives as one intact line.
func TestConcurrentAppendsDoNotCorruptLines(t *testing.T) {
	s := openStore(t)
	at := ts("2026-09-19T10:00:00Z")

	const writers, perWriter = 8, 10 // ~1.5 KiB each: one flush per writer
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var recs []any
			for i := 0; i < perWriter; i++ {
				recs = append(recs, measure(
					fmt.Sprintf("repo-%d", w), "coverage",
					at.Add(time.Duration(w*perWriter+i)*time.Second),
					float64(w*perWriter+i),
				))
			}
			if _, _, _, err := s.Append(stream(t, recs...)); err != nil {
				t.Errorf("writer %d: %v", w, err)
			}
		}(w)
	}
	wg.Wait()

	// Every line is a complete, parseable record.
	body := readFile(t, filepath.Join(s.Root, "measures", "2026", "09", "19.jsonl"))
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("partition holds %d lines, want %d — lines were interleaved", len(lines), writers*perWriter)
	}
	for i, l := range lines {
		var probe schema.Measure
		if err := json.Unmarshal([]byte(l), &probe); err != nil {
			t.Fatalf("line %d is not an intact record (%v): %.120q", i+1, err, l)
		}
	}

	// And every record is readable back, exactly once.
	got, err := s.QueryMeasures(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != writers*perWriter {
		t.Fatalf("read back %d records, want %d", len(got), writers*perWriter)
	}
	values := map[float64]int{}
	for _, m := range got {
		values[m.Value]++
	}
	for i := 0; i < writers*perWriter; i++ {
		if values[float64(i)] != 1 {
			t.Errorf("value %d appears %d times, want exactly once", i, values[float64(i)])
		}
	}
}

// SUSPECT: store.go:90 — schema.NewEncoder wraps a 4 KiB bufio.Writer, and a
// single Append that emits more than that flushes MID-RECORD. Two concurrent
// Appends to the same partition therefore splice a partial line into each
// other's output and both records are lost (the decoder skips them). Observed:
// ~40 of 1600 records vanish with 8 concurrent writers of 200 records each.
// The bytes are all still there — O_APPEND never overwrites — so nothing is
// destroyed, but the line framing is broken and records disappear silently.
// A per-partition mutex, or a flush aligned to record boundaries, would fix it.
//
// The splice depends on scheduling (it does not reproduce at GOMAXPROCS=1), so
// this test asserts only the deterministic half — no byte is lost — and reports
// any record loss rather than failing on it.
func TestLargeConcurrentAppendsCanSpliceLines(t *testing.T) {
	s := openStore(t)
	at := ts("2026-09-19T10:00:00Z")

	const writers, perWriter = 6, 120 // well over the 4 KiB buffer per writer
	build := func(w int) []any {
		var recs []any
		for i := 0; i < perWriter; i++ {
			m := measure(fmt.Sprintf("repo-%d", w), "coverage", at.Add(time.Duration(i)*time.Second), float64(i))
			m.Path = strings.Repeat("x", 100)
			recs = append(recs, m)
		}
		return recs
	}

	// Size the expected output by encoding the same records sequentially.
	var sized bytes.Buffer
	for w := 0; w < writers; w++ {
		enc := schema.NewEncoder(&sized)
		for _, r := range build(w) {
			if err := enc.Write(r); err != nil {
				t.Fatal(err)
			}
		}
		if err := enc.Flush(); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			if _, _, _, err := s.Append(stream(t, build(w)...)); err != nil {
				t.Errorf("writer %d: %v", w, err)
			}
		}(w)
	}
	wg.Wait()

	info, err := os.Stat(filepath.Join(s.Root, "measures", "2026", "09", "19.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(sized.Len()) {
		t.Errorf("partition is %d bytes, want %d — O_APPEND should mean no writer overwrites another",
			info.Size(), sized.Len())
	}

	got, err := s.QueryMeasures(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if want := writers * perWriter; len(got) != want {
		t.Logf("SUSPECT reproduced: %d of %d records unreadable after concurrent large appends "+
			"(mid-record buffer flush spliced the lines)", want-len(got), want)
	}
}

// ---- rollup --------------------------------------------------------------

func TestRollupAggregatesPerPeriod(t *testing.T) {
	s := openStore(t)
	// Monday 2026-09-14 through Sunday 2026-09-20, plus the next week.
	vals := map[string]float64{
		"2026-09-14T09:00:00Z": 10,
		"2026-09-16T09:00:00Z": 4,
		"2026-09-20T09:00:00Z": 7, // Sunday: still the week beginning the 14th
		"2026-09-21T09:00:00Z": 1, // Monday: a new week
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		appendAll(t, s, measure("billing-api", "coverage", ts(k), vals[k]))
	}

	weeks, err := s.Rollup(Query{}, "week")
	if err != nil {
		t.Fatal(err)
	}
	if len(weeks) != 2 {
		t.Fatalf("got %d weekly buckets, want 2: %+v", len(weeks), weeks)
	}
	w := weeks[0]
	if !w.Start.Equal(ts("2026-09-14T00:00:00Z")) {
		t.Errorf("week starts %s, want Monday 2026-09-14", w.Start.Format(time.RFC3339))
	}
	if w.N != 3 || w.Min != 4 || w.Max != 10 || w.Last != 7 {
		t.Errorf("bucket = %+v, want n=3 min=4 max=10 last=7", w)
	}
	if got := w.Mean; got < 6.99 || got > 7.01 {
		t.Errorf("mean = %v, want 7", got)
	}

	days, err := s.Rollup(Query{}, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 4 {
		t.Errorf("got %d daily buckets, want 4", len(days))
	}
	months, err := s.Rollup(Query{}, "month")
	if err != nil {
		t.Fatal(err)
	}
	if len(months) != 1 || !months[0].Start.Equal(ts("2026-09-01T00:00:00Z")) {
		t.Errorf("monthly buckets = %+v, want one starting 2026-09-01", months)
	}
}

// `Last` is the value the trend line should use: where the codebase was left at
// the end of the period, not an average over a period during which it moved.
func TestRollupLastIsTheFinalValueInThePeriod(t *testing.T) {
	s := openStore(t)
	day := ts("2026-09-19T00:00:00Z")
	// Appended newest-first to prove Last follows time, not insertion order.
	for i := 3; i >= 0; i-- {
		appendAll(t, s, measure("r", "coverage", day.Add(time.Duration(i)*time.Hour), float64(i*10)))
	}
	got, err := s.Rollup(Query{}, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d buckets, want 1", len(got))
	}
	if got[0].Last != 30 {
		t.Errorf("Last = %v, want 30 (the latest measurement of the day)", got[0].Last)
	}
}

func TestRollupSortsByPeriodThenRepoThenMetric(t *testing.T) {
	s := openStore(t)
	d1, d2 := ts("2026-09-19T09:00:00Z"), ts("2026-09-20T09:00:00Z")
	appendAll(t, s,
		measure("zeta", "coverage", d2, 1),
		measure("alpha", "lint", d1, 2),
		measure("alpha", "coverage", d1, 3),
		measure("zeta", "coverage", d1, 4),
	)
	got, err := s.Rollup(Query{}, "day")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2026-09-19|alpha|coverage", "2026-09-19|alpha|lint",
		"2026-09-19|zeta|coverage", "2026-09-20|zeta|coverage",
	}
	for i, b := range got {
		key := b.Start.Format("2006-01-02") + "|" + b.Repo + "|" + b.Metric
		if i >= len(want) || key != want[i] {
			t.Errorf("bucket %d = %s, want %s", i, key, want[i])
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d buckets, want %d", len(got), len(want))
	}
}

// ---- path parsing --------------------------------------------------------

func TestDateFromPathRejectsAnythingNotADatePartition(t *testing.T) {
	root := filepath.Join("x", "measures")
	cases := map[string]bool{
		filepath.Join(root, "2026", "09", "19.jsonl"): true,
		filepath.Join(root, "2026", "13", "19.jsonl"): false, // no month 13
		filepath.Join(root, "2026", "09", "32.jsonl"): false,
		filepath.Join(root, "2026", "09.jsonl"):       false, // too shallow
		filepath.Join(root, "a", "b", "c.jsonl"):      false,
		filepath.Join(root, "backup.jsonl"):           false,
	}
	for p, wantOK := range cases {
		d, ok := dateFromPath(root, p)
		if ok != wantOK {
			t.Errorf("dateFromPath(%s) ok = %v, want %v", p, ok, wantOK)
		}
		if wantOK && !d.Equal(ts("2026-09-19T00:00:00Z")) {
			t.Errorf("dateFromPath(%s) = %s, want 2026-09-19", p, d)
		}
	}
}

// A stray file that is not a date partition must still be read — dropping it
// would silently hide records someone moved in by hand.
func TestNonDatePartitionsAreStillScanned(t *testing.T) {
	s := openStore(t)
	appendAll(t, s, measure("r", "m", ts("2026-09-19T10:00:00Z"), 1))

	b, _ := json.Marshal(schema.Measure{
		V: 1, Kind: schema.KindMeasure, Repo: "r", TS: ts("2026-09-19T11:00:00Z"),
		Scope: schema.ScopeProject, Metric: "m", Value: 2,
	})
	if err := os.WriteFile(filepath.Join(s.Root, "measures", "imported.jsonl"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryMeasures(Query{Since: ts("2026-09-19T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d records, want 2 including the hand-placed file", len(got))
	}
}

// REGRESSION. Append used to call time.Now() per record, so every finding in
// one scan got its own timestamp. precision.go identifies "the latest scan" by
// exact timestamp equality, so a scan of N findings landed as N separate scans
// and N-1 of them immediately read as FIXED — manufacturing passive
// confirmation, in the numerator of the headline precision figure.
//
// One ingest is one event. All records from a single Append must share a stamp.
func TestAppendStampsOneTimestampForTheWholeCall(t *testing.T) {
	s := openStore(t)

	var recs []any
	for i := 0; i < 200; i++ {
		// Unstamped, exactly as a scan emits them.
		recs = append(recs, finding("svc", "no-panic", fmt.Sprintf("fp-%03d", i), time.Time{}))
	}
	if _, n, _, err := s.Append(stream(t, recs...)); err != nil || n != 200 {
		t.Fatalf("append: n=%d err=%v", n, err)
	}

	stamps := map[time.Time]int{}
	total := 0
	err := filepath.Walk(filepath.Join(s.Root, "findings"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return schema.Decode(f, func(rec schema.Record) error {
			if rec.Finding != nil {
				stamps[rec.Finding.TS]++
				total++
			}
			return nil
		}, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 200 {
		t.Fatalf("read back %d findings, want 200", total)
	}
	if len(stamps) != 1 {
		t.Errorf("one Append produced %d distinct timestamps, want 1 — "+
			"precision would read %d of these findings as fixed on ingest",
			len(stamps), total-1)
	}
	for ts := range stamps {
		if ts.IsZero() {
			t.Error("records stored with a zero timestamp")
		}
		if ts.UTC() != ts {
			t.Errorf("timestamp not normalised to UTC: %v", ts)
		}
	}
}
