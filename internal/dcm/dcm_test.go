package dcm

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/verdict"
	"github.com/sherzing/assay/pkg/schema"
)

const dartSrc = `import 'dart:async';

class Foo {
  // quality:false-positive the map is genuinely heterogeneous here
  dynamic bag = {};

  int add(int a, int b) {
    return a + b;
  }

  int sub(int a, int b) {
    return a - b;
  }
}
`

// report builds a document in the shape the DCM docs describe, with every
// line number shifted by off so tests can move the code without moving the
// findings' identity.
func report(off int) string {
	n := func(base int) string { return strconv.Itoa(base + off) }
	r := strings.NewReplacer("@5", n(5), "@7", n(7), "@8", n(8), "@9", n(9), "@11", n(11), "@13", n(13))
	return r.Replace(`{"formatVersion":13,"timestamp":"2026-09-22 10:00:00",
"analyzeResults":[{"path":"lib/a.dart","issues":[
 {"id":"avoid-dynamic","message":"Avoid using dynamic type.","location":{"startLine":@5,"startColumn":3,"endLine":@5,"endColumn":10},"severity":"warning","effortInMinutes":5},
 {"id":"prefer-returning-conditional","message":"Prefer returning the result directly.","location":{"startLine":@8,"startColumn":5,"endLine":@8,"endColumn":18},"severity":"style"}]}],
"metricResults":[{"path":"lib/a.dart","issues":[
 {"id":"cyclomatic-complexity","message":"","location":{"startLine":@7,"startColumn":3,"endLine":@9,"endColumn":4},"level":"none","threshold":20,"value":1,"declarationName":"Foo.add"},
 {"id":"source-lines-of-code","message":"","location":{"startLine":@7,"startColumn":3,"endLine":@9,"endColumn":4},"level":"none","threshold":50,"value":3,"declarationName":"Foo.add"},
 {"id":"cyclomatic-complexity","message":"This method has a cyclomatic complexity of 25, which exceeds the maximum of 20 allowed.","location":{"startLine":@11,"startColumn":3,"endLine":@13,"endColumn":4},"level":"very high","threshold":20,"value":25,"declarationName":"Foo.sub"},
 {"id":"weight-of-class","message":"","location":{"startLine":3,"startColumn":1,"endLine":@13,"endColumn":2},"level":"none","threshold":0.33,"value":0.5,"declarationName":"Foo"},
 {"id":"number-of-imports","message":"","location":{"startLine":1,"startColumn":1,"endLine":1,"endColumn":1},"level":"none","threshold":10,"value":1}]}],
"unusedFilesResults":[{"path":"lib/old.dart","issues":[{"id":"unused-file-issue","message":"Unused file","effortInMinutes":5}]}],
"unusedCodeResults":[{"path":"lib/a.dart","issues":[{"id":"unused-code-issue","message":"Unused method sub","location":{"startLine":@11,"startColumn":7,"endLine":@11,"endColumn":10},"declarationName":"sub","declarationType":"method"}]}],
"duplicationResults":[{"path":"lib/a.dart","issues":[{"id":"duplication-issue","message":"This method has 1 duplicate declaration","location":{"startLine":@7,"startColumn":3,"endLine":@9,"endColumn":4},"declarationName":"add","declarationType":"method","duplications":[{"declarationName":"plus","declarationType":"method","location":{"startLine":20,"startColumn":3,"endLine":22,"endColumn":4},"relativePath":"lib/b.dart"}]}]}],
"summary":[{"title":"Scanned files","value":2}]}`)
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T, off int) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "lib/a.dart", strings.Repeat("// shifted down\n", off)+dartSrc)
	write(t, root, "lib/old.dart", "// nothing imports this\n")
	return root
}

func imp(t *testing.T, off int, opt Options) *Result {
	t.Helper()
	res, err := Import(strings.NewReader(report(off)), opt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return res
}

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

func measure(t *testing.T, res *Result, scope schema.Scope, path, metric string) float64 {
	t.Helper()
	for _, m := range res.Measures {
		if m.Scope == scope && m.Path == path && m.Metric == metric {
			return m.Value
		}
	}
	t.Fatalf("no measure %s %q %s", scope, path, metric)
	return 0
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

func TestMeasuresScopesNamesAndRollups(t *testing.T) {
	res := imp(t, 0, Options{Root: setup(t, 0)})

	if v := measure(t, res, schema.ScopeFunction, "lib/a.dart:Foo.add", "cyclomatic"); v != 1 {
		t.Errorf("Foo.add cyclomatic = %v, want 1", v)
	}
	if v := measure(t, res, schema.ScopeFunction, "lib/a.dart:Foo.add", "sloc"); v != 3 {
		t.Errorf("Foo.add sloc = %v, want 3", v)
	}
	if v := measure(t, res, schema.ScopeFunction, "lib/a.dart:Foo.sub", "cyclomatic"); v != 25 {
		t.Errorf("Foo.sub cyclomatic = %v, want 25", v)
	}
	if v := measure(t, res, schema.ScopeClass, "lib/a.dart:Foo", "weight"); v != 0.5 {
		t.Errorf("Foo weight = %v, want 0.5", v)
	}
	if v := measure(t, res, schema.ScopeFile, "lib/a.dart", "imports"); v != 1 {
		t.Errorf("imports = %v, want 1", v)
	}

	// Project roll-ups use the Go scan's names, so one trend query serves both.
	if v := measure(t, res, schema.ScopeProject, "", "cyclomatic.max"); v != 25 {
		t.Errorf("cyclomatic.max = %v, want 25", v)
	}
	if v := measure(t, res, schema.ScopeProject, "", "cyclomatic.p50"); v != 1 {
		t.Errorf("cyclomatic.p50 = %v, want 1", v)
	}
	if v := measure(t, res, schema.ScopeProject, "", "cyclomatic.mean"); v != 13 {
		t.Errorf("cyclomatic.mean = %v, want 13", v)
	}
	if v := measure(t, res, schema.ScopeProject, "", "funcs"); v != 2 || res.Funcs != 2 {
		t.Errorf("funcs = %v / %d, want 2 (classes are not functions)", v, res.Funcs)
	}
	if v := measure(t, res, schema.ScopeProject, "", "files"); v != 2 || res.Files != 2 {
		t.Errorf("files = %v / %d, want 2", v, res.Files)
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

// The reason this importer reads source: a fingerprint keyed on the reported
// line would turn every reformat into a wave of new violations.
func TestFingerprintsFollowTheCodeNotTheLine(t *testing.T) {
	same := imp(t, 0, Options{Root: setup(t, 0)})
	moved := imp(t, 2, Options{Root: setup(t, 2)})
	for _, a := range same.Findings {
		b := byRule(t, moved, a.Rule)
		if a.Fingerprint != b.Fingerprint {
			t.Errorf("%s: fingerprint changed when the code moved: %s -> %s", a.Rule, a.Fingerprint, b.Fingerprint)
		}
		if b.Line != a.Line+2 && a.Line != 0 {
			t.Errorf("%s: line = %d, want %d", a.Rule, b.Line, a.Line+2)
		}
	}

	// Identity is the region DCM reported — here the `dynamic` token — so a
	// rename outside it keeps the fingerprint, and an edit inside it does not.
	root := setup(t, 0)
	write(t, root, "lib/a.dart", strings.Replace(dartSrc, "dynamic bag", "dynamic sack", 1))
	renamed := imp(t, 0, Options{Root: root})
	if byRule(t, same, "dcm:avoid-dynamic").Fingerprint != byRule(t, renamed, "dcm:avoid-dynamic").Fingerprint {
		t.Error("renaming the variable next to the offending span changed the fingerprint")
	}
	write(t, root, "lib/a.dart", strings.Replace(dartSrc, "dynamic bag", "Dynamic bag", 1))
	edited := imp(t, 0, Options{Root: root})
	if byRule(t, same, "dcm:avoid-dynamic").Fingerprint == byRule(t, edited, "dcm:avoid-dynamic").Fingerprint {
		t.Error("a different offending span produced the same fingerprint")
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

func TestSniffFormatVersionAndUnknownSections(t *testing.T) {
	if !Sniff([]byte(report(0))) {
		t.Error("a DCM report was not recognised")
	}
	if Sniff([]byte(`{"version":"2.1.0","runs":[]}`)) {
		t.Error("a SARIF document was mistaken for DCM")
	}
	if _, err := Import(strings.NewReader(`{"version":"2.1.0","runs":[]}`), Options{}); err == nil ||
		!strings.Contains(err.Error(), "formatVersion") {
		t.Errorf("SARIF fed to the DCM importer: err = %v, want a formatVersion complaint", err)
	}
	if _, err := Import(strings.NewReader(`not json`), Options{}); err == nil {
		t.Error("garbage imported without error")
	}

	// A section this importer has never heard of still lands under a readable
	// rule name, so a new DCM command does not vanish from the gate.
	res, err := Import(strings.NewReader(`{"formatVersion":11,
	  "fooBarResults":[{"path":"lib/x.dart","issues":[{"id":"thing","message":"m","location":{"startLine":1,"startColumn":1}}]}]}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FormatVersion != 11 {
		t.Errorf("formatVersion = %d, want 11 reported for the caller to warn about", res.FormatVersion)
	}
	if f := byRule(t, res, "dcm:foo-bar:thing"); f.File != "lib/x.dart" {
		t.Errorf("unknown section finding = %+v", f)
	}
}

// Identical findings in one function are numbered, so the ratchet tracks how
// many there are without ever depending on a line number.
func TestIdenticalFindingsGetOrdinals(t *testing.T) {
	root := t.TempDir()
	write(t, root, "lib/a.dart", "class Foo {\n  dynamic a;\n  dynamic b;\n  dynamic c;\n}\n")
	doc := func(lines ...int) string {
		var issues []string
		for _, l := range lines {
			issues = append(issues, `{"id":"avoid-dynamic","message":"m","location":{"startLine":`+strconv.Itoa(l)+`,"startColumn":3,"endLine":`+strconv.Itoa(l)+`,"endColumn":10},"severity":"warning"}`)
		}
		return `{"formatVersion":13,"analyzeResults":[{"path":"lib/a.dart","issues":[` + strings.Join(issues, ",") + `]}]}`
	}
	three, err := Import(strings.NewReader(doc(2, 3, 4)), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range three.Findings {
		if seen[f.Fingerprint] {
			t.Fatalf("identical findings still share a fingerprint: %+v", three.Findings)
		}
		seen[f.Fingerprint] = true
	}
	two, err := Import(strings.NewReader(doc(2, 3)), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// Dropping the last instance keeps the first two identities: one "fixed".
	for _, f := range two.Findings {
		if !seen[f.Fingerprint] {
			t.Errorf("fingerprint %s changed when the third instance was removed", f.Fingerprint)
		}
	}
	if len(two.Findings) != 2 || two.Findings[0].Fingerprint == two.Findings[1].Fingerprint {
		t.Errorf("two instances = %+v", two.Findings)
	}
}

func TestIssuesAsSingleObjectIsAccepted(t *testing.T) {
	res, err := Import(strings.NewReader(`{"formatVersion":13,
	  "analyzeResults":[{"path":"lib/x.dart","issues":{"id":"r","message":"m","location":{"startLine":2},"severity":"error"}}]}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f := byRule(t, res, "dcm:r"); f.Severity != model.Error || f.Line != 2 {
		t.Errorf("object-shaped issues = %+v", f)
	}
}

// A rule about the whole file, such as avoid-long-files, is reported at 1:1
// spanning to the end. Its identity must be the file, not the first line's
// text, or a new header comment reads as one finding fixed and one new.
func TestFileLevelFindingsSurviveHeaderChanges(t *testing.T) {
	doc := func(last int) string {
		return `{"formatVersion":13,"analyzeResults":[{"path":"lib/a.dart","issues":[{"id":"avoid-long-files","message":"Avoid long files.","location":{"startLine":1,"startColumn":1,"endLine":` + strconv.Itoa(last) + `,"endColumn":1},"severity":"warning"}]}]}`
	}
	root := t.TempDir()
	write(t, root, "lib/a.dart", dartSrc)
	before, err := Import(strings.NewReader(doc(15)), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "lib/a.dart", "// a new header comment\n"+dartSrc)
	after, err := Import(strings.NewReader(doc(16)), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if before.Findings[0].Fingerprint != after.Findings[0].Fingerprint {
		t.Error("a file-level finding changed identity when the file's first line changed")
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

// Per-function records in the Go scan's shape are what give a Dart baseline
// real caps and a `--json` report that reads like a native one.
func TestFunctionsCarryGoShapedMetrics(t *testing.T) {
	res := imp(t, 0, Options{Root: setup(t, 0)})
	if len(res.Functions) != 2 {
		t.Fatalf("functions = %+v, want Foo.add and Foo.sub", res.Functions)
	}
	add := res.Functions[0]
	if add.Name != "Foo.add" || add.File != "lib/a.dart" || add.Line != 7 || add.Cyclomatic != 1 || add.Statements != 3 || !add.Exported {
		t.Errorf("Foo.add = %+v", add)
	}
	if sub := res.Functions[1]; sub.Cyclomatic != 25 || sub.Cognitive != 0 {
		t.Errorf("Foo.sub = %+v; cognitive must stay 0, DCM has no such metric", sub)
	}
}

var update = flag.Bool("update", false, "rewrite testdata/demo/golden.json from the current importer output")

// demoConfig mirrors testdata/demo/.quality.yaml, which the CLI reads itself.
var demoConfig = verdict.Config{Org: "demo", Verdicts: []verdict.Rule{
	{Rule: "dcm:no-magic-number", Path: "lib/pricing.dart", Verdict: schema.WontFix, Reason: "the tax table is data, not magic"},
}}

// A real DCM 1.39 report over testdata/demo, a small synthetic package with
// deliberate smells. The golden file pins what this importer makes of it:
// every finding with its fingerprint and verdict, every measure, and the
// per-function records. If it changes, either DCM's format moved or a
// fingerprint did, and both deserve a deliberate decision rather than a
// silent `-update`.
func TestRealReportGolden(t *testing.T) {
	root := filepath.Join("testdata", "demo")
	res, err := ImportFile(filepath.Join(root, "report.json"), Options{Root: root, Config: demoConfig})
	if err != nil {
		t.Fatal(err)
	}
	if res.FormatVersion != Version {
		t.Errorf("fixture is formatVersion %d, importer pinned at %d", res.FormatVersion, Version)
	}
	got, err := json.MarshalIndent(struct {
		Files, Funcs int
		Findings     []model.Finding
		Functions    []model.FuncMetrics
		Measures     []schema.Measure
	}{res.Files, res.Funcs, res.Findings, res.Functions, res.Measures}, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join(root, "golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("importer output for the real report changed: diff %s against a run with -update, then decide whether the change is intended", golden)
	}

	// The facts a reader should take from the fixture, in words.
	seen := map[string]bool{}
	for _, f := range res.Findings {
		if seen[f.Fingerprint] {
			t.Errorf("duplicate fingerprint %s on %s %s:%d", f.Fingerprint, f.Rule, f.File, f.Line)
		}
		seen[f.Fingerprint] = true
	}
	if f := findAt(t, res, "dcm:avoid-dynamic", "lib/cart.dart", 9); f.Verdict != "false-positive" || f.VerdictFrom != "comment" {
		t.Errorf("the marker above `dynamic bag` was not harvested: %+v", f)
	}
	if f := findAt(t, res, "dcm:avoid-dynamic", "lib/cart.dart", 5); f.Verdict != "" {
		t.Errorf("the global `dynamic` has no marker and no config pattern, yet: %+v", f)
	}
	if f := findAt(t, res, "dcm:no-magic-number", "lib/pricing.dart", 0); f.Verdict != "wont-fix" || f.VerdictFrom != "config" {
		t.Errorf("the .quality.yaml pattern for lib/pricing.dart was not applied: %+v", f)
	}
	if f := findAt(t, res, "dcm:no-magic-number", "lib/cart.dart", 0); f.Verdict != "" {
		t.Errorf("the .quality.yaml pattern is scoped to lib/pricing.dart, yet: %+v", f)
	}
	if f := byRule(t, res, "dcm:code-duplication"); !strings.Contains(f.Message, "also lib/cart.dart:") {
		t.Errorf("duplication should name the other copy: %q", f.Message)
	}
	if f := byRule(t, res, "dcm:unused-files"); f.Line != 0 {
		t.Errorf("an unused file has no line, got %d", f.Line)
	}
	if v := measure(t, res, schema.ScopeProject, "", "cyclomatic.max"); v < 4 {
		t.Errorf("cyclomatic.max = %v, the fixture's total() branches more than that", v)
	}
}
