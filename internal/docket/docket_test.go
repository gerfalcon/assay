package docket

import (
	"strings"
	"testing"
	"time"

	"github.com/sherzing/assay/pkg/schema"
)

func f(rule, file, fp string) schema.Finding {
	return schema.Finding{
		Rule: rule, File: file, Fingerprint: fp,
		Severity: schema.SevError, Message: "m", Repo: "r",
	}
}

// Ninety naked type assertions is one ticket and one agent PR, not ninety
// tickets. If this regresses, the tool files a backlog nobody will ever clear.
func TestThemeGroupingCollapsesRepetition(t *testing.T) {
	var fs []schema.Finding
	for i := 0; i < 90; i++ {
		fs = append(fs, f("naked-type-assertion", "a.go", string(rune('a'+i%26))+string(rune('0'+i/26))))
	}
	fs = append(fs, f("error-swallowed", "b.go", "zz1"))

	p := BuildPlan(fs, nil, nil, Options{GroupBy: ByTheme})
	if len(p.Groups) != 2 {
		t.Fatalf("got %d groups, want 2 (one per rule)", len(p.Groups))
	}
	// Biggest cohort first: it clears the most findings per unit of review.
	if len(p.Groups[0].Fingerprints) != 90 {
		t.Errorf("largest group has %d findings, want 90", len(p.Groups[0].Fingerprints))
	}
	if !strings.Contains(p.Groups[0].Title, "90 ×") {
		t.Errorf("title does not state the count: %q", p.Groups[0].Title)
	}
}

func TestGroupByFileAndFinding(t *testing.T) {
	fs := []schema.Finding{
		f("r1", "a.go", "1"), f("r2", "a.go", "2"), f("r1", "b.go", "3"),
	}
	if g := BuildPlan(fs, nil, nil, Options{GroupBy: ByFile}).Groups; len(g) != 2 {
		t.Errorf("by file: got %d groups, want 2", len(g))
	}
	if g := BuildPlan(fs, nil, nil, Options{GroupBy: ByFinding}).Groups; len(g) != 3 {
		t.Errorf("by finding: got %d groups, want 3", len(g))
	}
}

// Ticketing a known false positive puts noise in front of a person, which is
// the exact failure this whole project exists to prevent.
func TestFalsePositivesAreNeverTicketed(t *testing.T) {
	fs := []schema.Finding{f("noisy-rule", "a.go", "fp1"), f("real-rule", "b.go", "ok1")}
	verdicts := map[string]schema.Verdict{
		"fp1": {Fingerprint: "fp1", Verdict: schema.FalsePositive, Reason: "idiomatic"},
	}
	p := BuildPlan(fs, verdicts, nil, Options{})
	if p.SkippedFP != 1 {
		t.Errorf("SkippedFP = %d, want 1", p.SkippedFP)
	}
	if len(p.Groups) != 1 || p.Groups[0].Findings[0].Rule != "real-rule" {
		t.Fatalf("false positive leaked into the plan: %+v", p.Groups)
	}
	if len(p.SkippedRules) != 1 || p.SkippedRules[0] != "noisy-rule" {
		t.Errorf("SkippedRules = %v, want [noisy-rule] so the rule can be fixed", p.SkippedRules)
	}
}

// Accepted and wont-fix are debt, not noise. They still deserve tickets.
func TestAcceptedAndWontFixAreStillTicketed(t *testing.T) {
	fs := []schema.Finding{f("r", "a.go", "1"), f("r", "b.go", "2")}
	verdicts := map[string]schema.Verdict{
		"1": {Fingerprint: "1", Verdict: schema.Accepted},
		"2": {Fingerprint: "2", Verdict: schema.WontFix},
	}
	p := BuildPlan(fs, verdicts, nil, Options{})
	if p.SkippedFP != 0 {
		t.Errorf("SkippedFP = %d, want 0 — only false-positive should be skipped", p.SkippedFP)
	}
	if len(p.Groups) != 1 || len(p.Groups[0].Fingerprints) != 2 {
		t.Errorf("accepted/wont-fix findings were dropped: %+v", p.Groups)
	}
}

// A re-run must never duplicate. This is the whole reason tickets key on
// fingerprints rather than on titles or line numbers.
func TestExistingTicketsPreventDuplicates(t *testing.T) {
	fs := []schema.Finding{f("r", "a.go", "1"), f("r", "b.go", "2")}
	existing := []schema.Ticket{{ID: "ENG-1", State: "open", Fingerprints: []string{"1"}}}

	p := BuildPlan(fs, nil, existing, Options{})
	if p.SkippedTicket != 1 {
		t.Errorf("SkippedTicket = %d, want 1", p.SkippedTicket)
	}
	if len(p.Groups) != 1 || len(p.Groups[0].Fingerprints) != 1 {
		t.Fatalf("already-ticketed finding was re-proposed: %+v", p.Groups)
	}
	if p.Groups[0].Fingerprints[0] != "2" {
		t.Errorf("wrong finding proposed: %v", p.Groups[0].Fingerprints)
	}
}

// Someone closing a ticket was a decision. Re-filing it overrides a human.
func TestClosedTicketsStillClaimTheirFindings(t *testing.T) {
	fs := []schema.Finding{f("r", "a.go", "1")}
	existing := []schema.Ticket{{ID: "ENG-1", State: "closed", Fingerprints: []string{"1"}}}
	if p := BuildPlan(fs, nil, existing, Options{}); len(p.Groups) != 0 {
		t.Errorf("re-filed a finding whose ticket a human closed: %+v", p.Groups)
	}
}

// The answer to "if I ticket by theme, how do I know what is still open".
func TestSyncReportsPartialProgress(t *testing.T) {
	ticket := schema.Ticket{ID: "ENG-1", Fingerprints: []string{"1", "2", "3", "4"}}
	current := []schema.Finding{f("r", "a.go", "1"), f("r", "b.go", "3")} // 2 and 4 fixed

	got := Sync([]schema.Ticket{ticket}, current)[0]
	if got.Total != 4 || got.Resolved != 2 {
		t.Errorf("progress = %d/%d, want 2/4", got.Resolved, got.Total)
	}
	if got.Done {
		t.Error("marked done with 2 findings still live")
	}
	if len(got.Remaining) != 2 {
		t.Errorf("Remaining = %v, want 2 entries", got.Remaining)
	}
}

func TestSyncClosesWhenCohortIsClear(t *testing.T) {
	ticket := schema.Ticket{ID: "ENG-1", Fingerprints: []string{"1", "2"}}
	got := Sync([]schema.Ticket{ticket}, []schema.Finding{f("r", "c.go", "9")})[0]
	if !got.Done || got.Resolved != 2 {
		t.Errorf("cohort fully fixed but Done=%v Resolved=%d", got.Done, got.Resolved)
	}
}

// An empty cohort must not read as "finished" — that would auto-close a ticket
// that never tracked anything.
func TestEmptyCohortIsNotDone(t *testing.T) {
	got := Sync([]schema.Ticket{{ID: "ENG-1"}}, nil)[0]
	if got.Done {
		t.Error("empty cohort reported as done")
	}
}

func TestLatestResolvesTheLog(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	all := []schema.Ticket{
		{Provider: "linear", ID: "ENG-1", State: "open", TS: t0},
		{Provider: "linear", ID: "ENG-1", State: "closed", TS: t0.Add(time.Minute)},
		{Provider: "linear", ID: "ENG-2", State: "open", TS: t0},
	}
	got := Latest(all)
	if len(got) != 2 {
		t.Fatalf("got %d tickets, want 2 distinct", len(got))
	}
	for _, tk := range got {
		if tk.ID == "ENG-1" && tk.State != "closed" {
			t.Errorf("ENG-1 resolved to %q, want the later 'closed' state", tk.State)
		}
	}
}

func TestMaxAndMinSize(t *testing.T) {
	fs := []schema.Finding{
		f("big", "a.go", "1"), f("big", "b.go", "2"), f("big", "c.go", "3"),
		f("small", "d.go", "4"),
	}
	if g := BuildPlan(fs, nil, nil, Options{MinSize: 2}).Groups; len(g) != 1 {
		t.Errorf("min-size: got %d groups, want 1", len(g))
	}
	if g := BuildPlan(fs, nil, nil, Options{Max: 1}).Groups; len(g) != 1 {
		t.Errorf("max: got %d groups, want 1", len(g))
	}
}

func TestSeverityFilter(t *testing.T) {
	fs := []schema.Finding{
		{Rule: "e", File: "a.go", Fingerprint: "1", Severity: schema.SevError, Message: "m"},
		{Rule: "i", File: "b.go", Fingerprint: "2", Severity: schema.SevInfo, Message: "m"},
	}
	g := BuildPlan(fs, nil, nil, Options{Severity: schema.SevWarn}).Groups
	if len(g) != 1 || g[0].Findings[0].Rule != "e" {
		t.Errorf("severity filter did not drop the info finding: %+v", g)
	}
}

// The body is what an engineer or agent actually works from.
func TestBodyIsActionable(t *testing.T) {
	fs := []schema.Finding{
		{Rule: "r", File: "a.go", Line: 10, Fingerprint: "1", Severity: schema.SevError,
			Message: "thing is wrong", Suggest: "do it this way", Symbol: "Foo"},
	}
	b := BuildPlan(fs, nil, nil, Options{}).Groups[0].Body
	for _, want := range []string{"thing is wrong", "do it this way", "a.go:10", "Foo", "ratchet check"} {
		if !strings.Contains(b, want) {
			t.Errorf("body missing %q:\n%s", want, b)
		}
	}
	if !strings.Contains(b, "fixed at creation") {
		t.Error("body does not explain that the cohort is bounded — that is what makes it closeable")
	}
}
