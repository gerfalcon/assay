package store

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sherzing/assay/pkg/schema"
)

// scan appends one scan of one repo. Every finding in a scan shares a single
// timestamp, because that is what findingHistory keys "the latest scan" on.
func scan(t *testing.T, s *Store, repo string, at time.Time, ruleName string, fps ...string) {
	t.Helper()
	recs := make([]any, 0, len(fps))
	for _, fp := range fps {
		recs = append(recs, finding(repo, ruleName, fp, at))
	}
	appendAll(t, s, recs...)
}

func judge(t *testing.T, s *Store, fp string, j schema.Judgement, org string) {
	t.Helper()
	appendAll(t, s, verdictRec(fp, "", org, j, ts("2026-09-25T10:00:00Z")))
}

func ruleRow(t *testing.T, s *Store, name string) RulePrecision {
	t.Helper()
	rows, err := s.Precision()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Rule == name {
			return r
		}
	}
	t.Fatalf("rule %q not in precision output: %+v", name, rows)
	return RulePrecision{}
}

func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

var (
	t1 = ts("2026-09-01T10:00:00Z")
	t2 = ts("2026-09-20T10:00:00Z")
	t3 = ts("2026-09-05T10:00:00Z")
)

// The three buckets mean different things and the whole project depends on not
// collapsing them: fixed is passive confirmation, carried is "real but nobody
// is paying it down", false-positive is the only one that counts against the
// rule. Unjudged is a fourth thing — no evidence either way — and must stay out
// of the precision fraction entirely.
func TestThreeBucketsPlusUnjudged(t *testing.T) {
	s := openStore(t)
	scan(t, s, "billing-api", t1, "no-context-todo", "fp-fixed", "fp-acc", "fp-wf", "fp-false", "fp-open")
	// A later scan of the same repo: fp-fixed has gone away, the rest remain.
	scan(t, s, "billing-api", t2, "no-context-todo", "fp-acc", "fp-wf", "fp-false", "fp-open")

	judge(t, s, "fp-acc", schema.Accepted, "acme")
	judge(t, s, "fp-wf", schema.WontFix, "acme")
	judge(t, s, "fp-false", schema.FalsePositive, "acme")

	r := ruleRow(t, s, "no-context-todo")
	if r.Fixed != 1 {
		t.Errorf("Fixed = %d, want 1 (the finding that stopped appearing)", r.Fixed)
	}
	if r.Carried != 2 {
		t.Errorf("Carried = %d, want 2 (accepted + wont-fix share a bucket)", r.Carried)
	}
	if r.FalsePositive != 1 {
		t.Errorf("FalsePositive = %d, want 1", r.FalsePositive)
	}
	if r.Unjudged != 1 {
		t.Errorf("Unjudged = %d, want 1 (still present, nobody has looked at it)", r.Unjudged)
	}
	if r.Judged != 4 || r.Seen != 5 {
		t.Errorf("Judged/Seen = %d/%d, want 4/5 — unjudged counts as seen but not judged", r.Judged, r.Seen)
	}
	if !closeTo(r.Precision, 0.75) {
		t.Errorf("Precision = %v, want 3 confirmed / 4 judged = 0.75", r.Precision)
	}
	if !closeTo(r.Coverage, 0.8) {
		t.Errorf("Coverage = %v, want 4/5 = 0.8", r.Coverage)
	}
	if !closeTo(r.Carriage, 2.0/3.0) {
		t.Errorf("Carriage = %v, want 2 carried / 3 confirmed", r.Carriage)
	}
	if r.Repos != 1 || r.Orgs != 1 {
		t.Errorf("Repos/Orgs = %d/%d, want 1/1", r.Repos, r.Orgs)
	}
}

// A false-positive verdict wins even while the finding is still on disk, and a
// carried verdict wins even after the finding disappears. The explicit human
// judgement is never overridden by the passive signal.
func TestExplicitVerdictBeatsThePassiveSignal(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", t1, "r", "gone-but-accepted", "gone-but-false", "keeper")
	scan(t, s, "repo-a", t2, "r", "keeper")
	judge(t, s, "gone-but-accepted", schema.Accepted, "acme")
	judge(t, s, "gone-but-false", schema.FalsePositive, "acme")

	r := ruleRow(t, s, "r")
	if r.Fixed != 0 {
		t.Errorf("Fixed = %d, want 0 — disappearing must not overrule an explicit verdict", r.Fixed)
	}
	if r.Carried != 1 || r.FalsePositive != 1 {
		t.Errorf("Carried/FalsePositive = %d/%d, want 1/1", r.Carried, r.FalsePositive)
	}
}

// THE SUBTLE ONE. "Still live" is judged against each repo's own most recent
// scan, not a global high-water mark. The same fingerprint can exist in two
// codebases; if repo A has been scanned since the fix and repo B has not been
// scanned for a fortnight, the finding is still live in B and must not be
// counted as confirmed-by-fix. Getting this wrong manufactures evidence out of
// nothing more than an idle CI job.
func TestStillLiveIsScopedPerRepoNotGlobally(t *testing.T) {
	// repo-b's latest scan (t3) is OLDER than repo-a's latest (t2), so a global
	// high-water mark would wrongly declare everything in repo-b fixed.
	s := openStore(t)
	scan(t, s, "repo-a", t1, "r", "shared", "gone", "keeper-a")
	scan(t, s, "repo-a", t2, "r", "keeper-a") // shared and gone are fixed HERE
	scan(t, s, "repo-b", t3, "r", "shared", "keeper-b")

	r := ruleRow(t, s, "r")
	if r.Fixed != 1 {
		t.Errorf("Fixed = %d, want 1 — only `gone` is fixed everywhere it was seen", r.Fixed)
	}
	if r.Unjudged != 3 {
		t.Errorf("Unjudged = %d, want 3 (shared, keeper-a, keeper-b)", r.Unjudged)
	}
	if r.Repos != 2 {
		t.Errorf("Repos = %d, want 2", r.Repos)
	}

	// Control: with repo-b removed, the very same fingerprint IS fixed. This is
	// what proves the assertion above is about the per-repo scoping and not
	// about some other property of the fixture.
	only := openStore(t)
	scan(t, only, "repo-a", t1, "r", "shared", "gone", "keeper-a")
	scan(t, only, "repo-a", t2, "r", "keeper-a")
	if r := ruleRow(t, only, "r"); r.Fixed != 2 {
		t.Errorf("without repo-b, Fixed = %d, want 2 (shared and gone)", r.Fixed)
	}
}

// Precision is a ratio of totals, not a mean of ratios. Averaging per-repo
// precision would let one tiny repo with two findings outvote a large one with
// two hundred.
func TestPrecisionIsRecomputedFromSummedCountsNotAveraged(t *testing.T) {
	s := openStore(t)
	// repo-a: 9 fixed, 1 false positive  -> 0.90 on its own
	fps := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9"}
	scan(t, s, "repo-a", t1, "r", append(append([]string{}, fps...), "a-false")...)
	scan(t, s, "repo-a", t2, "r", "a-false")
	judge(t, s, "a-false", schema.FalsePositive, "acme")

	// repo-b: 1 fixed, 3 false positives -> 0.25 on its own
	scan(t, s, "repo-b", t1, "r", "b1", "b-false1", "b-false2", "b-false3")
	scan(t, s, "repo-b", t2, "r", "b-false1", "b-false2", "b-false3")
	for _, fp := range []string{"b-false1", "b-false2", "b-false3"} {
		judge(t, s, fp, schema.FalsePositive, "globex")
	}

	r := ruleRow(t, s, "r")
	if r.Fixed != 10 || r.FalsePositive != 4 || r.Judged != 14 {
		t.Fatalf("counts = fixed %d, fp %d, judged %d; want 10/4/14", r.Fixed, r.FalsePositive, r.Judged)
	}
	if !closeTo(r.Precision, 10.0/14.0) {
		t.Errorf("Precision = %v, want %v (10 confirmed / 14 judged)", r.Precision, 10.0/14.0)
	}
	if mean := (0.9 + 0.25) / 2; closeTo(r.Precision, mean) {
		t.Errorf("Precision = %v, which is the mean of the per-repo rates — it must be recomputed from the totals", r.Precision)
	}
	if r.Repos != 2 || r.Orgs != 2 {
		t.Errorf("Repos/Orgs = %d/%d, want 2/2", r.Repos, r.Orgs)
	}
}

// An unexamined rule has not earned a number. Zero is an honest answer here;
// 1.0 would be a flattering lie that could promote the rule.
func TestRuleWithNoJudgedFindingsGetsNoPrecision(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", t1, "unexamined", "x1", "x2")

	r := ruleRow(t, s, "unexamined")
	if r.Judged != 0 || r.Precision != 0 {
		t.Errorf("Judged/Precision = %d/%v, want 0/0", r.Judged, r.Precision)
	}
	if r.Unjudged != 2 || r.Seen != 2 {
		t.Errorf("Unjudged/Seen = %d/%d, want 2/2", r.Unjudged, r.Seen)
	}
	if r.Coverage != 0 || r.Carriage != 0 {
		t.Errorf("Coverage/Carriage = %v/%v, want 0/0", r.Coverage, r.Carriage)
	}
}

// A fingerprint seen on twenty days is one piece of evidence, not twenty.
func TestRepeatedAppearancesCountOnce(t *testing.T) {
	s := openStore(t)
	for d := 1; d <= 5; d++ {
		at := time.Date(2026, 9, d, 10, 0, 0, 0, time.UTC)
		scan(t, s, "repo-a", at, "r", "fp1", "fp2")
	}
	r := ruleRow(t, s, "r")
	if r.Seen != 2 {
		t.Errorf("Seen = %d, want 2 — evidence is per fingerprint, not per sighting", r.Seen)
	}
	if r.Fixed != 0 {
		t.Errorf("Fixed = %d, want 0 — both are in the latest scan", r.Fixed)
	}
}

// A rule can be renamed. The verdict carries the name the human judged under,
// and that is the one the evidence should accrue to.
func TestVerdictRuleOverridesTheFindingRule(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", t1, "old-name", "fp1")
	appendAll(t, s, verdictRec("fp1", "new-name", "acme", schema.Accepted, t2))

	rows, err := s.Precision()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Rule != "new-name" {
		t.Fatalf("rows = %+v, want a single row for new-name", rows)
	}
	if rows[0].Carried != 1 {
		t.Errorf("Carried = %d, want 1", rows[0].Carried)
	}
}

// Org attribution comes only from verdicts, because only a human judging a
// finding says which organisation vouched for it. A rule whose evidence is all
// passive fixes has no org behind it, and so cannot clear the multi-org bar.
func TestOrgsComeFromVerdictsOnly(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", t1, "r", "fp1", "fp2", "fp3")
	scan(t, s, "repo-a", t2, "r", "fp2", "fp3")
	judge(t, s, "fp2", schema.Accepted, "acme")
	judge(t, s, "fp3", schema.Accepted, "") // judged, but unattributed

	r := ruleRow(t, s, "r")
	if r.Fixed != 1 || r.Carried != 2 {
		t.Fatalf("fixed/carried = %d/%d, want 1/2", r.Fixed, r.Carried)
	}
	if r.Orgs != 1 {
		t.Errorf("Orgs = %d, want 1 — a blank org must not count as an organisation", r.Orgs)
	}
}

// A verdict about a fingerprint that was never stored as a finding is not
// evidence about any rule: there is nothing to say it was ever raised.
func TestVerdictForUnknownFingerprintIsIgnored(t *testing.T) {
	s := openStore(t)
	appendAll(t, s, verdictRec("never-seen", "r", "acme", schema.FalsePositive, t1))
	rows, err := s.Precision()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
}

func TestFindingsWithNoRuleAreSkipped(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", t1, "", "fp1")
	scan(t, s, "repo-a", t1, "r", "fp2")
	rows, err := s.Precision()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Rule != "r" {
		t.Errorf("rows = %+v, want only the rule that has a name", rows)
	}
}

// Ordering is the report's reading order: strongest evidence first, ties broken
// by name so output is stable.
func TestPrecisionSortsByEvidenceThenName(t *testing.T) {
	s := openStore(t)
	// bbb and aab each end up with 2 judged, aaa with 1.
	scan(t, s, "repo-a", t1, "bbb", "b1", "b2")
	scan(t, s, "repo-a", t1, "aab", "c1", "c2")
	scan(t, s, "repo-a", t1, "aaa", "d1")
	scan(t, s, "repo-a", t2, "bbb", "keeper")
	for _, fp := range []string{"b1", "b2", "c1", "c2", "d1"} {
		judge(t, s, fp, schema.Accepted, "acme")
	}

	rows, err := s.Precision()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Rule)
	}
	want := []string{"aab", "bbb", "aaa"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("order = %v, want %v (judged desc, then rule asc)", got, want)
		}
	}
}

// SUSPECT: precision.go:182-201 — "the latest scan" is identified by EXACT
// timestamp equality, so every finding in one scan must carry a byte-identical
// `ts`. Nothing guarantees that: store.go:107 stamps each timestamp-less
// finding with its own time.Now(), so a scan of N findings lands as N distinct
// "scans" and only the last one is live. The other N-1 are counted as Fixed the
// instant they are ingested — passive confirmation the rule never earned, in
// the numerator of the headline precision figure. Grouping by scan (commit, or
// the max timestamp per repo-day) rather than by exact instant would fix it.
// This asserts the current behaviour.
func TestFindingsInOneScanWithDriftingTimestampsReadAsFixed(t *testing.T) {
	s := openStore(t)
	base := ts("2026-09-19T10:00:00Z")
	// One logical scan, but each finding stamped a millisecond apart — exactly
	// the shape Append produces for findings that arrive without a timestamp.
	appendAll(t, s,
		finding("repo-a", "r", "fp1", base),
		finding("repo-a", "r", "fp2", base.Add(time.Millisecond)),
		finding("repo-a", "r", "fp3", base.Add(2*time.Millisecond)),
	)

	r := ruleRow(t, s, "r")
	if r.Fixed != 2 || r.Unjudged != 1 {
		t.Errorf("fixed/unjudged = %d/%d, want 2/1 (current behaviour: only the last-stamped finding is live)",
			r.Fixed, r.Unjudged)
	}
	if !closeTo(r.Precision, 1.0) {
		t.Errorf("Precision = %v, want 1.0 — nothing was fixed, yet the rule scores perfectly", r.Precision)
	}
}

// SUSPECT: precision.go:172 — findingInfo.repo is only set the first time a
// fingerprint is seen, so a fingerprint occurring in several repos is
// attributed to whichever partition was read first. Repos is therefore an
// undercount, and Repos feeds the promotion bar (evidence/promote.go:145,
// MinRepos defaults to 3), so a rule confirmed across many codebases by the
// same recurring pattern can be held back. Asserting the current behaviour.
func TestRepoCountAttributesAFingerprintToItsFirstRepoOnly(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", t1, "r", "shared")
	scan(t, s, "repo-b", t2, "r", "shared")

	r := ruleRow(t, s, "r")
	if r.Repos != 1 {
		t.Errorf("Repos = %d; current behaviour counts only the first repo a fingerprint was seen in", r.Repos)
	}
	if r.Seen != 1 {
		t.Errorf("Seen = %d, want 1 — the fingerprint is the unit of evidence", r.Seen)
	}
}

// SUSPECT: precision.go:92 — Precision reads verdicts only from the verdict
// log. schema.Finding carries its own Verdict field ("travels WITH the finding,
// so a consumer needs no store"), and nothing on the ingest path
// (store.go:105-110, cmd/strata/main.go:132) converts it into a verdict record.
// A findings stream that arrives already judged — an import, another team's
// export, a SARIF conversion — therefore counts as unjudged, or worse as fixed
// once it stops appearing. Asserting the current behaviour.
func TestVerdictCarriedOnTheFindingIsNotCounted(t *testing.T) {
	s := openStore(t)
	f := finding("repo-a", "r", "fp1", t1)
	f.Verdict = schema.FalsePositive
	f.VerdictWhy = "the rule is wrong about generated code"
	appendAll(t, s, f)

	r := ruleRow(t, s, "r")
	if r.FalsePositive != 0 || r.Unjudged != 1 {
		t.Errorf("fp/unjudged = %d/%d, want 0/1; the verdict on the finding itself is ignored",
			r.FalsePositive, r.Unjudged)
	}
}

// A damaged findings partition must not take the whole precision report with
// it: the surviving evidence is still worth reporting.
func TestPrecisionSurvivesACorruptFindingsPartition(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", ts("2026-09-18T10:00:00Z"), "r", "fp1")
	writeRaw(t, s, "findings", "2026/09/19", "{\"kind\":\"finding\",\n garbage\n")

	rows, err := s.Precision()
	if err != nil {
		t.Fatalf("Precision failed on a corrupt partition: %v", err)
	}
	if len(rows) != 1 || rows[0].Seen != 1 {
		t.Errorf("rows = %+v, want the one surviving finding", rows)
	}
}

// Findings partitions are read from a shared stream format, so a measure that
// was piped into the wrong directory is ignored rather than counted.
func TestNonFindingRecordsInAFindingsPartitionAreIgnored(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", t1, "r", "fp1")
	body := stream(t, measure("repo-a", "coverage", t1, 1), verdictRec("fp9", "r", "acme", schema.Accepted, t1))
	writeRaw(t, s, "findings", "2026/09/19", body.String())

	r := ruleRow(t, s, "r")
	if r.Seen != 1 {
		t.Errorf("Seen = %d, want 1 — only findings are evidence", r.Seen)
	}
}

// A findings partition the decoder cannot scan at all must fail loudly: a
// silently short evidence corpus is worse than no answer.
func TestPrecisionFailsOnAnUnscannableFindingsPartition(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", t1, "r", "fp1")
	writeRaw(t, s, "findings", "2026/09/19", strings.Repeat("x", 9<<20)+"\n")

	if _, err := s.Precision(); err == nil {
		t.Error("Precision returned no error for an unscannable findings partition")
	}
}

// An unreadable verdict log must surface as an error rather than as a report
// that quietly claims every finding is unjudged.
func TestPrecisionPropagatesAnUnreadableVerdictLog(t *testing.T) {
	s := openStore(t)
	scan(t, s, "repo-a", t1, "r", "fp1")
	// A directory where the log should be: it opens, but it cannot be read.
	if err := os.MkdirAll(filepath.Join(s.Root, "verdicts", "verdicts.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Precision(); err == nil {
		t.Error("Precision returned no error for an unreadable verdict log")
	}
}
