package learn

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func rates(m map[string]float64) Result {
	r := Result{Rates: m, Candidate: Candidate{ID: "x", Severity: "WARNING"}}
	classify(&r, DefaultThresholds())
	return r
}

// Thresholds were tuned against real measurements, so the classifier has to
// reproduce the calls made by hand on two production Go services.
func TestClassifyMatchesTheRealMeasurements(t *testing.T) {
	cases := []struct {
		name string
		a, b float64
		want Verdict
	}{
		{"never used anywhere", 0.00, 0.00, Held},
		{"vanishingly rare", 0.02, 0.02, Held},
		{"rare but present", 0.13, 0.00, Held},
		{"panic in library", 0.11, 0.05, Held},
		{"one avoids it entirely", 0.29, 0.00, Divergent},
		{"interface{} vs any", 21.13, 0.07, Divergent},
		{"naked return", 3.77, 4.48, NotHeld},
		{"os.Exit", 0.39, 1.23, NotHeld},
		{"bare goroutine", 0.42, 0.63, NotHeld},
		{"global mutable state", 0.68, 1.62, NotHeld},
	}
	for _, c := range cases {
		got := rates(map[string]float64{"a": c.a, "b": c.b}).Verdict
		if got != c.want {
			t.Errorf("%s (%.2f, %.2f) = %q, want %q", c.name, c.a, c.b, got, c.want)
		}
	}
}

// The rejections are the point. A probe set where everything passes proves the
// method does not discriminate.
func TestCatalogueIncludesPatternsExpectedToFail(t *testing.T) {
	want := map[string]bool{
		"no-naked-return": true, "no-os-exit-outside-main": true,
		"no-global-mutable-state": true, "no-bare-goroutine": true,
	}
	for _, c := range Catalogue() {
		delete(want, c.ID)
	}
	if len(want) > 0 {
		t.Errorf("catalogue is missing deliberately-failing probes: %v", want)
	}
}

func TestEveryCandidateIsActionable(t *testing.T) {
	for _, c := range Catalogue() {
		if c.Why == "" {
			t.Errorf("%s has no explanation of what is wrong", c.ID)
		}
		if c.Fix == "" {
			t.Errorf("%s does not say what to do instead", c.ID)
		}
		if c.Pattern == nil {
			t.Errorf("%s has no probe pattern", c.ID)
		}
		if c.Severity == "" {
			t.Errorf("%s has no severity", c.ID)
		}
	}
}

// A single codebase cannot distinguish a shared convention from local habit, so
// a lone zero must not read as strong evidence.
func TestSingleRepoStillClassifies(t *testing.T) {
	if got := rates(map[string]float64{"only": 0.0}).Verdict; got != Held {
		t.Errorf("single repo at 0.00 classified %q, want held", got)
	}
	if got := rates(map[string]float64{"only": 5.0}).Verdict; got != NotHeld {
		t.Errorf("single repo at 5.00 classified %q, want not-held", got)
	}
}

func TestProbeCountsAndRates(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 3 matches across 10 lines -> 300 per kLOC
	write("a.go", "package p\n\nfunc a() {\n\tpanic(\"x\")\n\tpanic(\"y\")\n}\n\nfunc b() {\n\tpanic(\"z\")\n}\n")
	// Tests must be excluded, or test norms swamp the production signal.
	write("a_test.go", "package p\n\nfunc t() {\n\tpanic(\"in a test\")\n\tpanic(\"and another\")\n}\n")

	cands := []Candidate{{ID: "p", Why: "w", Fix: "f", Severity: "WARNING",
		Pattern: regexp.MustCompile(`panic\(`)}}
	res, err := Probe([]string{dir}, cands, DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(dir)
	if got := res[0].Counts[name]; got != 3 {
		t.Errorf("count = %d, want 3 — test files should not be counted", got)
	}
	if got := res[0].Rates[name]; got < 200 || got > 400 {
		t.Errorf("rate = %.1f, want roughly 300 per kLOC", got)
	}
}

func TestProbeErrorsOnEmptyRepo(t *testing.T) {
	if _, err := Probe([]string{t.TempDir()}, Catalogue(), DefaultThresholds()); err == nil {
		t.Error("a directory with no Go source should error, not report every rate as zero")
	}
}

// A rule shipped without its evidence is what this whole exercise exists to
// prevent, so the generator must not be able to produce one.
func TestDraftRuleCarriesItsEvidence(t *testing.T) {
	r := Result{
		Candidate: Candidate{ID: "no-thing", Why: "it breaks", Fix: "do the other thing",
			Severity: "ERROR", Semgrep: "thing(...)"},
		Rates: map[string]float64{"a": 0.01, "b": 0.00},
	}
	out := DraftRule(r)
	for _, want := range []string{
		"no-thing", "ERROR", "It breaks", "Do the other thing",
		"evidence:", "0.01/kLOC", "confidence: medium", "thing(...)", "*_test.go",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("draft rule missing %q:\n%s", want, out)
		}
	}
}

func TestResultsAreOrderedHeldFirst(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte(
		"package p\nfunc a() {\n\treturn\n}\n"), 0o644)
	res, err := Probe([]string{dir}, Catalogue(), DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[Verdict]bool{}
	last := -1
	for _, r := range res {
		o := order(r.Verdict)
		if o < last {
			t.Fatalf("results out of order: %q after a later verdict", r.Candidate.ID)
		}
		last = o
		seen[r.Verdict] = true
	}
}
