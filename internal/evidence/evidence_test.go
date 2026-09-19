package evidence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sherzing/assay/internal/store"
)

func good() Record {
	return Record{
		Schema: SchemaID, Rule: "no-context-todo", Org: "acme", Period: "2026-Q3",
		Repos: 4, Fixed: 31, Carried: 6, FalsePositive: 3, Judged: 40, Precision: 0.93,
	}
}

func verify(t *testing.T, v any) []Problem {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return Verify(raw)
}

func TestCleanRecordPasses(t *testing.T) {
	if p := verify(t, good()); Fatal(p) {
		t.Errorf("a clean record was rejected: %v", p)
	}
}

// THE LOAD-BEARING TEST. An allowlist means a future exporter that adds a field
// cannot leak it past an old verifier. A blocklist would wave all of these
// through.
func TestUnknownFieldsAreRejected(t *testing.T) {
	leaks := map[string]any{
		"file":         "internal/impl/postgres/pool.go",
		"fingerprints": []string{"9f2a3c4d5e6f7081"},
		"repoNames":    []string{"billing-api", "orders-worker"},
		"reason":       "DEBT-412 needs the v2 migration",
		"by":           "sven.herzing@acme.com",
		"commit":       "15b279b2b8086cc90dc1ab8b3a26203126075dc7",
		"paths":        []string{"cmd/api/main.go"},
		"anythingNew":  "whatever a future version decides to add",
	}
	for field, val := range leaks {
		m := map[string]any{}
		raw, _ := json.Marshal(good())
		_ = json.Unmarshal(raw, &m)
		m[field] = val

		out, _ := json.Marshal(m)
		probs := Verify(out)
		if !Fatal(probs) {
			t.Errorf("field %q passed verification — an allowlist must reject it", field)
			continue
		}
		found := false
		for _, p := range probs {
			if p.Field == field {
				found = true
			}
		}
		if !found {
			t.Errorf("field %q was rejected but not named in the problems", field)
		}
	}
}

// A permitted field can still carry something it should not — rule ids and org
// names are free text.
func TestAllowedFieldsAreScannedForLeaks(t *testing.T) {
	cases := map[string]Record{
		"path in rule":        func() Record { r := good(); r.Rule = "internal/impl/pool.go"; return r }(),
		"go file in rule":     func() Record { r := good(); r.Rule = "check-main.go"; return r }(),
		"fingerprint in rule": func() Record { r := good(); r.Rule = "9f2a3c4d5e6f7081"; return r }(),
		"ticket in org":       func() Record { r := good(); r.Org = "acme DEBT-412"; return r }(),
		"email in org":        func() Record { r := good(); r.Org = "sven@acme.com"; return r }(),
	}
	for name, rec := range cases {
		if !Fatal(verify(t, rec)) {
			t.Errorf("%s: passed verification but should be rejected", name)
		}
	}
}

// A precise timestamp, with an org and a rule, narrows who you are far more
// than the counts do.
func TestPeriodMustBeCoarse(t *testing.T) {
	for _, bad := range []string{
		"2026-09-19T15:04:05Z", "2026-09-19", "2026-09", "Q3", "2026", "",
	} {
		r := good()
		r.Period = bad
		if !Fatal(verify(t, r)) {
			t.Errorf("period %q accepted, want only YYYY-Qn", bad)
		}
	}
	for _, ok := range []string{"2026-Q1", "2026-Q4", "2025-Q2"} {
		r := good()
		r.Period = ok
		if Fatal(verify(t, r)) {
			t.Errorf("period %q rejected but is the required form", ok)
		}
	}
}

func TestCountsMustReconcile(t *testing.T) {
	r := good()
	r.Judged = 99 // does not equal fixed+carried+fp
	probs := verify(t, r)
	if !Fatal(probs) {
		t.Error("inconsistent counts accepted")
	}
	r = good()
	r.Fixed = -1
	if !Fatal(verify(t, r)) {
		t.Error("negative count accepted")
	}
}

func TestUnattributedEvidenceIsRejected(t *testing.T) {
	r := good()
	r.Org = ""
	if !Fatal(verify(t, r)) {
		t.Error("evidence with no org accepted — it cannot count toward the multi-org bar")
	}
}

func TestWrongSchemaRejected(t *testing.T) {
	r := good()
	r.Schema = "assay-evidence/99"
	if !Fatal(verify(t, r)) {
		t.Error("unknown schema version accepted")
	}
}

// Weak evidence should be flagged but not blocked — partial evidence beats none.
func TestThinEvidenceWarnsButPasses(t *testing.T) {
	r := good()
	r.Fixed, r.Carried, r.FalsePositive, r.Judged = 2, 1, 0, 3
	r.Repos = 1
	probs := verify(t, r)
	if Fatal(probs) {
		t.Errorf("thin evidence was blocked, should only warn: %v", probs)
	}
	if len(probs) < 2 {
		t.Errorf("expected warnings about low count and single repo, got %v", probs)
	}
}

func TestBuildDropsEverythingIdentifying(t *testing.T) {
	rows := []store.RulePrecision{{
		Rule: "r1", Fixed: 20, Carried: 5, FalsePositive: 5, Judged: 30,
		Precision: 0.8333333, Repos: 3, Orgs: 1, Unjudged: 100,
	}}
	recs, _, err := Build(rows, Options{Org: "acme", Period: "2026-Q3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	raw, _ := json.Marshal(recs[0])
	// The only defence that survives someone adding a field to store.RulePrecision.
	for _, forbidden := range []string{"unjudged", "orgs", "coverage", "carriage", "seen"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Errorf("export leaked field %q: %s", forbidden, raw)
		}
	}
	if recs[0].Precision != 0.83 {
		t.Errorf("precision = %v, want it rounded to 0.83", recs[0].Precision)
	}
}

func TestBuildRefusesWithoutOrg(t *testing.T) {
	if _, _, err := Build(nil, Options{}); err == nil {
		t.Error("Build accepted an empty org")
	}
}

func TestBuildSkipsThinRules(t *testing.T) {
	rows := []store.RulePrecision{
		{Rule: "thick", Fixed: 20, Judged: 20, Repos: 2},
		{Rule: "thin", Fixed: 2, Judged: 2, Repos: 1},
	}
	recs, skipped, err := Build(rows, Options{Org: "acme", MinJudged: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Rule != "thick" {
		t.Errorf("got %+v, want only the thick rule", recs)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "thin") {
		t.Errorf("skipped = %v, want the thin rule named so it is not silently dropped", skipped)
	}
}

func TestQuarter(t *testing.T) {
	cases := map[string]string{
		"2026-01-15": "2026-Q1", "2026-03-31": "2026-Q1",
		"2026-04-01": "2026-Q2", "2026-09-19": "2026-Q3", "2026-12-31": "2026-Q4",
	}
	for in, want := range cases {
		d, _ := time.Parse("2006-01-02", in)
		if got := Quarter(d); got != want {
			t.Errorf("Quarter(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestVerifyStreamReportsPerLine(t *testing.T) {
	ok, _ := json.Marshal(good())
	bad := good()
	bad.Org = ""
	badRaw, _ := json.Marshal(bad)
	data := string(ok) + "\n# a comment\n\n" + string(badRaw) + "\n"

	probs := VerifyStream([]byte(data))
	if _, hasLine1 := probs[1]; hasLine1 {
		t.Error("clean line 1 reported as a problem")
	}
	if _, hasLine4 := probs[4]; !hasLine4 {
		t.Errorf("bad record on line 4 not reported: %v", probs)
	}
}
