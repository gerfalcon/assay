package dcm

import (
	"strings"
	"testing"

	"github.com/sherzing/assay/pkg/schema"
)

func TestMeasuresScopesNamesAndRollups(t *testing.T) {
	res := imp(t, 0, Options{Root: setup(t, 0)})
	if res.FormatVersion != Version {
		t.Errorf("formatVersion = %d, want %d", res.FormatVersion, Version)
	}
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
	// A metric this importer has never heard of still imports, dashes as dots,
	// and a value DCM happened to quote is still a number.
	if v := measure(t, res, schema.ScopeFunction, "lib/a.dart:Foo.add", "some.new.metric"); v != 2.5 {
		t.Errorf("unknown metric = %v, want 2.5", v)
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
	if v := measure(t, res, schema.ScopeProject, "", "files"); v != 1 || res.Files != 1 {
		t.Errorf("files = %v / %d, want 1", v, res.Files)
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

// Several reports, such as one per package, merge into one result whose
// roll-ups cover the union rather than repeating once per report.
func TestMergeRecomputesRollups(t *testing.T) {
	other := strings.ReplaceAll(strings.ReplaceAll(report(0), "lib/a.dart", "lib/b.dart"), `"value":25`, `"value":9`)
	b, err := Import(strings.NewReader(other), Options{})
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(imp(t, 0, Options{}), b)
	if merged.Funcs != 4 || merged.Files != 2 {
		t.Errorf("funcs = %d files = %d, want 4 and 2", merged.Funcs, merged.Files)
	}
	if v := measure(t, merged, schema.ScopeProject, "", "cyclomatic.max"); v != 25 {
		t.Errorf("cyclomatic.max = %v, want 25", v)
	}
	n := 0
	for _, m := range merged.Measures {
		if m.Scope == schema.ScopeProject && m.Metric == "cyclomatic.p90" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("cyclomatic.p90 appears %d times, want once", n)
	}
	if one := Merge(imp(t, 0, Options{})); one.Funcs != 2 {
		t.Errorf("a single report merges to itself, got funcs = %d", one.Funcs)
	}
}
