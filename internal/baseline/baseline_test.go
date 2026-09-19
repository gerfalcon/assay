package baseline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sherzing/assay/internal/model"
)

func finding(rule, file, fn, snip string) model.Finding {
	return model.Finding{
		Rule: rule, File: file, Func: fn, Severity: model.Error,
		Fingerprint: model.Fingerprint(file, rule, fn, snip),
	}
}

func reportWith(fs ...model.Finding) *model.Report {
	r := &model.Report{Findings: fs}
	r.Summarise(1)
	return r
}

// The central promise: pre-existing violations are tolerated, new ones fail.
// If this breaks, the tool becomes unadoptable on any real codebase.
func TestExistingToleratedNewFails(t *testing.T) {
	old := finding("naked-type-assertion", "a.go", "F", "x.(string)")
	base := From(reportWith(old), "abc123")

	// Same code again: no regression.
	res := base.Check(reportWith(old), false)
	if res.Regressed() {
		t.Errorf("unchanged code reported a regression: %+v", res.New)
	}
	if res.Existing != 1 {
		t.Errorf("existing = %d, want 1", res.Existing)
	}

	// A new violation appears alongside the tolerated one.
	fresh := finding("error-swallowed", "b.go", "G", "_ = err")
	res = base.Check(reportWith(old, fresh), false)
	if !res.Regressed() {
		t.Fatal("new finding did not trigger a regression")
	}
	if len(res.New) != 1 || res.New[0].Rule != "error-swallowed" {
		t.Errorf("New = %+v, want the error-swallowed finding", res.New)
	}
	if res.Existing != 1 {
		t.Errorf("existing = %d, want 1 (the tolerated one)", res.Existing)
	}
}

// Fixing something must be visible. A ratchet that only ever says "no" reads as
// policing; reporting what got fixed is what makes it feel like progress.
func TestFixedIsReported(t *testing.T) {
	a := finding("naked-type-assertion", "a.go", "F", "x.(string)")
	b := finding("error-swallowed", "b.go", "G", "_ = err")
	base := From(reportWith(a, b), "")

	res := base.Check(reportWith(a), false)
	if res.Regressed() {
		t.Errorf("removing a finding should never be a regression: %+v", res.New)
	}
	if len(res.Fixed) != 1 || res.Fixed[0].Rule != "error-swallowed" {
		t.Errorf("Fixed = %+v, want the error-swallowed entry", res.Fixed)
	}
}

func TestTightenDropsOnlyFixed(t *testing.T) {
	a := finding("naked-type-assertion", "a.go", "F", "x.(string)")
	b := finding("error-swallowed", "b.go", "G", "_ = err")
	base := From(reportWith(a, b), "")

	removed := base.Tighten(reportWith(a))
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if len(base.Tolerated) != 1 {
		t.Errorf("tolerated = %d, want 1", len(base.Tolerated))
	}
	// The still-present finding must remain tolerated after tightening.
	if res := base.Check(reportWith(a), false); res.Regressed() {
		t.Errorf("tightening wrongly un-tolerated a live finding: %+v", res.New)
	}
}

// Caps guard the gap the fingerprint set cannot see: brand-new code that is
// complex but trips no rule.
func TestStrictCapsCatchComplexityGrowth(t *testing.T) {
	rep := reportWith()
	rep.Funcs = []model.FuncMetrics{{Cyclomatic: 5, Cognitive: 3, MaxNesting: 2}}
	rep.Summarise(1)
	base := From(rep, "")

	worse := reportWith()
	worse.Funcs = []model.FuncMetrics{{Cyclomatic: 40, Cognitive: 60, MaxNesting: 9}}
	worse.Summarise(1)

	if res := base.Check(worse, false); res.Regressed() {
		t.Error("caps must be advisory unless strict-caps is set")
	}
	res := base.Check(worse, true)
	if !res.Regressed() {
		t.Fatal("strict caps did not catch a complexity increase")
	}
	if len(res.CapBreak) != 3 {
		t.Errorf("capBreaches = %d, want 3", len(res.CapBreak))
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	a := finding("naked-type-assertion", "a.go", "F", "x.(string)")
	base := From(reportWith(a), "deadbeef")
	path := filepath.Join(t.TempDir(), "b.json")

	if err := base.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Commit != "deadbeef" || len(got.Tolerated) != 1 {
		t.Errorf("round trip lost data: %+v", got)
	}
	if res := got.Check(reportWith(a), false); res.Regressed() {
		t.Error("loaded baseline failed to tolerate its own recorded finding")
	}
}

func TestLoadRejectsWrongVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	if err := writeFile(path, `{"version":"999","tolerated":{}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected a version mismatch to be rejected, not silently accepted")
	}
}

func writeFile(path, s string) error {
	return os.WriteFile(path, []byte(s), 0o644)
}
