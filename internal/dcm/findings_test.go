package dcm

import (
	"strings"
	"testing"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/verdict"
	"github.com/sherzing/assay/pkg/schema"
)

func byRule(t *testing.T, res *Result, rule string) model.Finding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Rule == rule {
			return f
		}
	}
	t.Fatalf("no finding with rule %q; have %v", rule, rules(res))
	return model.Finding{}
}

func rules(res *Result) []string {
	var out []string
	for _, f := range res.Findings {
		out = append(out, f.Rule)
	}
	return out
}

// findAt picks one finding by rule, file and line; line 0 means any line.
func findAt(t *testing.T, res *Result, rule, file string, line int) model.Finding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Rule == rule && f.File == file && (line == 0 || f.Line == line) {
			return f
		}
	}
	t.Fatalf("no %s finding in %s at line %d", rule, file, line)
	return model.Finding{}
}

func TestFindingsRulesSeveritiesAndAttribution(t *testing.T) {
	res := imp(t, 0, Options{Root: setup(t, 0)})
	if res.FormatVersion != Version {
		t.Errorf("formatVersion = %d, want %d", res.FormatVersion, Version)
	}
	if len(res.Findings) != 5 {
		t.Fatalf("got %d findings %v, want 5", len(res.Findings), rules(res))
	}

	dup := byRule(t, res, "dcm:code-duplication")
	if dup.Func != "add" || !strings.Contains(dup.Message, "also lib/b.dart:20 (plus)") {
		t.Errorf("duplication should name the other copy: %+v", dup)
	}

	dyn := byRule(t, res, "dcm:avoid-dynamic")
	if dyn.Severity != model.Warn || dyn.File != "lib/a.dart" || dyn.Line != 5 || dyn.Col != 3 {
		t.Errorf("avoid-dynamic = %+v", dyn)
	}
	// Line 5 sits inside class Foo, whose range the class metric revealed.
	if dyn.Func != "Foo" {
		t.Errorf("avoid-dynamic attributed to %q, want the enclosing class Foo", dyn.Func)
	}
	if dyn.Verdict != "false-positive" || dyn.VerdictFrom != "comment" || !strings.Contains(dyn.VerdictWhy, "heterogeneous") {
		t.Errorf("marker on the line above not honoured: %+v", dyn)
	}

	// Line 8 is inside Foo.add, the innermost declaration with metrics.
	cond := byRule(t, res, "dcm:prefer-returning-conditional")
	if cond.Severity != model.Info || cond.Func != "Foo.add" {
		t.Errorf("style issue = severity %q func %q, want info inside Foo.add", cond.Severity, cond.Func)
	}

	old := byRule(t, res, "dcm:unused-files")
	if old.File != "lib/old.dart" || old.Line != 0 || old.Severity != model.Warn || old.Fingerprint == "" {
		t.Errorf("unused file = %+v", old)
	}

	sub := byRule(t, res, "dcm:unused-code:method")
	if sub.Func != "sub" || sub.Severity != model.Warn || sub.Line != 11 {
		t.Errorf("unused method = %+v", sub)
	}

	for _, f := range res.Findings {
		if strings.HasPrefix(f.Rule, "dcm:metrics:") {
			t.Errorf("metric breach became a finding by default: %s", f.Rule)
		}
	}
}

func TestMetricBreachesAsFindingsOnRequest(t *testing.T) {
	res := imp(t, 0, Options{Root: setup(t, 0), MetricsAsFindings: true})
	f := byRule(t, res, "dcm:metrics:cyclomatic-complexity")
	if f.Func != "Foo.sub" || f.Line != 11 || f.Severity != model.Warn {
		t.Errorf("breach finding = %+v", f)
	}
	if len(res.Findings) != 6 {
		t.Errorf("got %d findings, want 6 (only the very-high breach is added, not the ok values)", len(res.Findings))
	}
}

func TestConfigVerdictsAndPrecedence(t *testing.T) {
	cfg := verdict.Config{Verdicts: []verdict.Rule{
		{Rule: "dcm:unused-files", Verdict: schema.WontFix, Reason: "kept for the demo", Until: "2030-01-01"},
		{Rule: "dcm:avoid-dynamic", Verdict: schema.Accepted, Reason: "config says accepted"},
	}}
	res := imp(t, 0, Options{Root: setup(t, 0), Config: cfg})

	old := byRule(t, res, "dcm:unused-files")
	if old.Verdict != "wont-fix" || old.VerdictFrom != "config" || old.VerdictUntil != "2030-01-01" {
		t.Errorf("config verdict not applied: %+v", old)
	}
	// The marker sits next to the code; it beats the pattern in config.
	if dyn := byRule(t, res, "dcm:avoid-dynamic"); dyn.Verdict != "false-positive" || dyn.VerdictFrom != "comment" {
		t.Errorf("comment should win over config: %+v", dyn)
	}
}

func TestWithoutSourceIdentityFallsBackToMessage(t *testing.T) {
	// No files on disk at all: findings still import, fingerprinted on the
	// message, and no marker can be read.
	res := imp(t, 0, Options{Root: t.TempDir()})
	if len(res.Findings) != 5 {
		t.Fatalf("got %d findings, want 5", len(res.Findings))
	}
	dyn := byRule(t, res, "dcm:avoid-dynamic")
	if dyn.Fingerprint == "" || dyn.Verdict != "" {
		t.Errorf("without source: %+v", dyn)
	}
}

// DCM's declaration types arrive as prose ("top level variable"); a rule ID
// with spaces breaks every comma-separated list of rules.
func TestDeclarationTypesAreKebabCasedInRuleIDs(t *testing.T) {
	res, err := Import(strings.NewReader(`{"formatVersion":13,"unusedCodeResults":[{"path":"lib/a.dart","issues":[{"id":"unused-code-issue","message":"Unused top level variable 'x'","location":{"startLine":1,"startColumn":1},"declarationName":"x","declarationType":"top level variable"}]}]}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f := res.Findings[0]; f.Rule != "dcm:unused-code:top-level-variable" {
		t.Errorf("rule = %q, want dcm:unused-code:top-level-variable", f.Rule)
	}
}

// A section this importer has never heard of still lands under a readable
// rule name, so a new DCM command does not vanish from the gate.
func TestUnknownSectionsStillImport(t *testing.T) {
	res, err := Import(strings.NewReader(`{"formatVersion":13,
	  "fooBarResults":[{"path":"lib/x.dart","issues":[{"id":"thing","message":"m","location":{"startLine":1,"startColumn":1}}]}]}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f := byRule(t, res, "dcm:foo-bar:thing"); f.File != "lib/x.dart" {
		t.Errorf("unknown section finding = %+v", f)
	}
}
