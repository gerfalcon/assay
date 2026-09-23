// The metrics section: DCM metric values as measures and per-function records.
package dcm

import (
	"sort"
	"strings"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/pkg/schema"
)

// names maps DCM metric IDs to the names the Go scan emits, so one lens query serves both.
var names = map[string]string{
	"cyclomatic-complexity":            "cyclomatic",
	"maximum-nesting-level":            "nesting",
	"number-of-parameters":             "params",
	"source-lines-of-code":             "sloc",
	"lines-of-code":                    "loc",
	"halstead-volume":                  "halstead.volume",
	"maintainability-index":            "maintainability",
	"number-of-used-widgets":           "widgets.used",
	"widgets-nesting-level":            "widgets.nesting",
	"number-of-methods":                "methods",
	"number-of-added-methods":          "methods.added",
	"number-of-overridden-methods":     "methods.overridden",
	"number-of-implemented-interfaces": "interfaces",
	"depth-of-inheritance-tree":        "inheritance.depth",
	"coupling-between-object-classes":  "coupling",
	"response-for-class":               "rfc",
	"tight-class-cohesion":             "cohesion",
	"weight-of-class":                  "weight",
	"weighted-methods-per-class":       "wmc",
	"number-of-imports":                "imports",
	"number-of-external-imports":       "imports.external",
	"technical-debt":                   "technical.debt",
}

var classLevel = map[string]bool{
	"number-of-methods": true, "number-of-added-methods": true, "number-of-overridden-methods": true,
	"number-of-implemented-interfaces": true, "depth-of-inheritance-tree": true,
	"coupling-between-object-classes": true, "response-for-class": true, "tight-class-cohesion": true,
	"weight-of-class": true, "weighted-methods-per-class": true,
}

var fileScoped = map[string]bool{
	"number-of-imports": true, "number-of-external-imports": true, "technical-debt": true,
}

func (im *importer) values(files []fileIssues) {
	seen := map[string]bool{}
	for _, f := range files {
		im.files[f.path] = true
		for _, is := range f.issues {
			im.value(f.path, is, seen)
		}
	}
}

// value records one measurement at its scope and, on request, a breach finding.
func (im *importer) value(path string, is issue, seen map[string]bool) {
	scope, mpath := scopeOf(is.ID, path, is.DeclarationName)
	if scope != schema.ScopeFile && is.Location.StartLine > 0 {
		im.index(path, is.DeclarationName, is.Location)
	}
	if scope == schema.ScopeFunction {
		im.funcs[mpath] = true
	}
	v, ok := num(is.Value)
	if !ok {
		return
	}
	name := nameOf(is.ID)
	if scope == schema.ScopeFunction {
		im.record(mpath, path, is, name, v)
	}
	key := string(scope) + "\x00" + mpath + "\x00" + name
	if seen[key] {
		return
	}
	seen[key] = true
	im.res.Measures = append(im.res.Measures, schema.Measure{Scope: scope, Path: mpath, Metric: name, Value: v})
	if im.opt.MetricsAsFindings && breached(is.Level) {
		im.breach(path, is)
	}
}

// record keeps per-function metrics in the Go scan's shape, so a Dart baseline gets real caps.
func (im *importer) record(key, path string, is issue, name string, v float64) {
	f, ok := im.fm[key]
	if !ok {
		short := is.DeclarationName
		if i := strings.LastIndex(short, "."); i >= 0 {
			short = short[i+1:]
		}
		f = &model.FuncMetrics{
			Name: is.DeclarationName, File: path, Line: is.Location.StartLine,
			Exported: !strings.HasPrefix(short, "_"),
		}
		im.fm[key] = f
	}
	switch name {
	case "cyclomatic":
		f.Cyclomatic = int(v)
	case "nesting":
		f.MaxNesting = int(v)
	case "sloc":
		f.Statements = int(v)
	case "params":
		f.Params = int(v)
	}
}

func nameOf(id string) string {
	if name, ok := names[id]; ok {
		return name
	}
	return strings.ReplaceAll(id, "-", ".")
}

// scopeOf places a metric at the level it measures; class metrics get the class scope.
func scopeOf(id, file, declName string) (schema.Scope, string) {
	switch {
	case fileScoped[id] || declName == "":
		return schema.ScopeFile, file
	case classLevel[id]:
		return schema.ScopeClass, file + ":" + declName
	}
	return schema.ScopeFunction, file + ":" + declName
}

// functions lists the per-function records in file order.
func (im *importer) functions() {
	for _, f := range im.fm {
		im.res.Functions = append(im.res.Functions, *f)
	}
	sort.Slice(im.res.Functions, func(i, j int) bool {
		a, b := im.res.Functions[i], im.res.Functions[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Name < b.Name
	})
}

// aggregate rolls function-scope values up to project scope under the Go scan's names.
func aggregate(res *Result) {
	byName := map[string][]float64{}
	for _, m := range res.Measures {
		if m.Scope == schema.ScopeFunction {
			byName[m.Metric] = append(byName[m.Metric], m.Value)
		}
	}
	keys := make([]string, 0, len(byName))
	for n := range byName {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	put := func(name string, v float64) {
		res.Measures = append(res.Measures, schema.Measure{Scope: schema.ScopeProject, Metric: name, Value: v})
	}
	for _, n := range keys {
		vals := byName[n]
		sort.Float64s(vals)
		sum := 0.0
		for _, v := range vals {
			sum += v
		}
		put(n+".max", vals[len(vals)-1])
		put(n+".mean", sum/float64(len(vals)))
		put(n+".p50", pct(vals, 0.50))
		put(n+".p90", pct(vals, 0.90))
	}
	put("files", float64(res.Files))
	put("funcs", float64(res.Funcs))
}

// pct matches model.pct: nearest-rank on a sorted slice.
func pct(sorted []float64, p float64) float64 {
	return sorted[int(p*float64(len(sorted)-1))]
}
