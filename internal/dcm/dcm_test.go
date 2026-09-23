package dcm

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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

// report builds a document in the shape the DCM docs describe, with every line
// number shifted by off so tests can move the code without moving identities.
// It carries every section DCM emits; a half of the importer that does not read
// a section must ignore it.
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
 {"id":"some-new-metric","message":"","location":{"startLine":@7,"startColumn":3,"endLine":@9,"endColumn":4},"level":"none","threshold":1,"value":"2.5","declarationName":"Foo.add"},
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

// setup writes the fixture's source, shifted down by off lines.
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

// skipUnlessImplemented lets the spec land before the importer: these tests
// describe the behaviour, and run for real once Import stops returning
// ErrNotImplemented.
func skipUnlessImplemented(t *testing.T) {
	t.Helper()
	if _, err := Import(strings.NewReader("{}"), Options{}); errors.Is(err, ErrNotImplemented) {
		t.Skip("importer not implemented yet; the spec is what this change adds")
	}
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

func TestSniffAndFormatVersion(t *testing.T) {
	skipUnlessImplemented(t)
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
	res, err := Import(strings.NewReader(`{"formatVersion":11}`), Options{})
	if err != nil || res.FormatVersion != 11 {
		t.Errorf("formatVersion = %d, err = %v; want 11 reported for the caller to warn about", res.FormatVersion, err)
	}
}

func TestIssuesAsSingleObjectIsAccepted(t *testing.T) {
	skipUnlessImplemented(t)
	res, err := Import(strings.NewReader(`{"formatVersion":13,
	  "metricResults":[{"path":"lib/x.dart","issues":{"id":"cyclomatic-complexity","location":{"startLine":2},"value":4,"declarationName":"f"}}]}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if v := measure(t, res, schema.ScopeFunction, "lib/x.dart:f", "cyclomatic"); v != 4 {
		t.Errorf("object-shaped issues = %v", v)
	}
}

var update = flag.Bool("update", false, "rewrite testdata/demo/golden.json from the current importer output")

// A real DCM 1.39 report over testdata/demo, a small synthetic package. The
// golden file pins what this importer makes of it. If it changes, either DCM's
// format moved or the mapping did, and both deserve a deliberate decision
// rather than a silent `-update`.
func TestRealReportGolden(t *testing.T) {
	skipUnlessImplemented(t)
	root := filepath.Join("testdata", "demo")
	res, err := ImportFile(filepath.Join(root, "report.json"), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if res.FormatVersion != Version {
		t.Errorf("fixture is formatVersion %d, importer pinned at %d", res.FormatVersion, Version)
	}
	got, err := json.MarshalIndent(struct {
		Files, Funcs int
		Functions    any
		Measures     []schema.Measure
	}{res.Files, res.Funcs, res.Functions, res.Measures}, "", " ")
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
	if len(res.Functions) != 9 || res.Funcs != 9 {
		t.Errorf("functions = %d / %d, want the fixture's 9", len(res.Functions), res.Funcs)
	}
	if v := measure(t, res, schema.ScopeProject, "", "cyclomatic.max"); v < 4 {
		t.Errorf("cyclomatic.max = %v, the fixture's total() branches more than that", v)
	}
	seen := map[string]bool{}
	for _, m := range res.Measures {
		key := string(m.Scope) + " " + m.Path + " " + m.Metric
		if seen[key] {
			t.Errorf("duplicate measure %s", key)
		}
		seen[key] = true
	}
}
