package evidence

import "testing"

func rec(rule, org, period string, repos, fixed, carried, fp int) Record {
	return Record{
		Schema: SchemaID, Rule: rule, Org: org, Period: period,
		Repos: repos, Fixed: fixed, Carried: carried, FalsePositive: fp,
		Judged: fixed + carried + fp,
	}
}

func assess(t *testing.T, recs ...Record) Assessment {
	t.Helper()
	c := Aggregate(recs)
	if len(c) != 1 {
		t.Fatalf("expected one rule, got %d", len(c))
	}
	for _, v := range c {
		return Assess(*v, DefaultThresholds())
	}
	return Assessment{}
}

func TestPromoteWhenEveryBarIsCleared(t *testing.T) {
	a := assess(t,
		rec("r", "acme", "2026-Q2", 2, 30, 2, 2),
		rec("r", "globex", "2026-Q3", 2, 25, 1, 3),
	)
	if a.Outcome != Promote {
		t.Errorf("outcome = %q, want promote. unmet: %v", a.Outcome, a.Unmet)
	}
}

// The case Sven identified: the rule is right, and nobody fixes what it finds
// because the underlying problem is one the ecosystem works around.
func TestHighCarriagePromotesAsInformational(t *testing.T) {
	a := assess(t,
		rec("framework-driver-quirk", "acme", "2026-Q2", 2, 2, 30, 1),
		rec("framework-driver-quirk", "globex", "2026-Q3", 2, 1, 25, 2),
	)
	if a.Outcome != PromoteInformational {
		t.Fatalf("outcome = %q, want promote-informational (carriage %.2f)", a.Outcome, a.Corpus.Carriage)
	}
	if len(a.Notes) == 0 {
		t.Error("no explanation given for the informational recommendation")
	}
}

// Rules must be able to lose, or the corpus only grows and its average quality
// only falls.
func TestEnoughEvidenceAndMostlyWrongIsRejected(t *testing.T) {
	a := assess(t,
		rec("noisy", "acme", "2026-Q2", 2, 10, 2, 30),
		rec("noisy", "globex", "2026-Q3", 2, 5, 1, 25),
	)
	if a.Outcome != Reject {
		t.Fatalf("outcome = %q, want reject (precision %.2f over %d judged)",
			a.Outcome, a.Corpus.Precision, a.Corpus.Judged)
	}
}

// Low precision on thin evidence is "not yet", not "reject" — you cannot
// conclude a rule is bad from a handful of findings.
func TestLowPrecisionOnThinEvidenceIsNotYet(t *testing.T) {
	a := assess(t, rec("new", "acme", "2026-Q3", 1, 1, 0, 3))
	if a.Outcome != NotYet {
		t.Errorf("outcome = %q, want not-yet — 4 findings cannot condemn a rule", a.Outcome)
	}
}

func TestUnmetCriteriaAreNamedWithTheGap(t *testing.T) {
	a := assess(t, rec("r", "acme", "2026-Q3", 1, 20, 0, 0))
	if a.Outcome != NotYet {
		t.Fatalf("outcome = %q, want not-yet", a.Outcome)
	}
	// One org, 20 judged, 1 repo — three bars missed, precision fine.
	if len(a.Unmet) != 3 {
		t.Errorf("unmet = %v, want 3 entries (orgs, judged, repos)", a.Unmet)
	}
	for _, u := range a.Unmet {
		if !contains(u, "needs") {
			t.Errorf("unmet entry does not say what is needed: %q", u)
		}
	}
}

// A tiny contributor with a lucky 1.00 must not drag up a rule that is mostly
// wrong everywhere else.
func TestPrecisionIsRecomputedNotAveraged(t *testing.T) {
	a := assess(t,
		rec("r", "tiny", "2026-Q3", 1, 2, 0, 0),  // precision 1.00 over 2
		rec("r", "big", "2026-Q3", 3, 10, 0, 40), // precision 0.20 over 50
	)
	// Averaging per-org would give 0.60; the honest figure is 12/52.
	if got := a.Corpus.Precision; got > 0.25 {
		t.Errorf("precision = %.2f, want ~0.23 — per-org averaging would have inflated it", got)
	}
	if a.Outcome != Reject {
		t.Errorf("outcome = %q, want reject", a.Outcome)
	}
}

func TestAggregateCountsDistinctOrgsAndPeriods(t *testing.T) {
	c := Aggregate([]Record{
		rec("r", "acme", "2026-Q2", 1, 5, 0, 0),
		rec("r", "acme", "2026-Q3", 2, 5, 0, 0), // same org, later period
		rec("r", "globex", "2026-Q3", 1, 5, 0, 0),
	})["r"]
	if len(c.Orgs) != 2 {
		t.Errorf("orgs = %v, want 2 distinct", c.Orgs)
	}
	if len(c.Periods) != 2 {
		t.Errorf("periods = %v, want 2 distinct", c.Periods)
	}
	if c.Repos != 4 || c.Judged != 15 {
		t.Errorf("repos=%d judged=%d, want 4 and 15", c.Repos, c.Judged)
	}
}

// A rule measured in one quarter is a weaker bet than one that held over time.
func TestSinglePeriodIsFlagged(t *testing.T) {
	a := assess(t,
		rec("r", "acme", "2026-Q3", 2, 30, 0, 2),
		rec("r", "globex", "2026-Q3", 2, 25, 0, 3),
	)
	found := false
	for _, n := range a.Notes {
		if contains(n, "single period") {
			found = true
		}
	}
	if !found {
		t.Errorf("no note about single-period evidence: %v", a.Notes)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
