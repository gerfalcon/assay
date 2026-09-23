// Package dcm ingests DCM (dcm.dev) JSON reports, so the ratchet, the history
// and the tickets work on Dart and Flutter code. DCM has no SARIF reporter, and
// SARIF would drop the metrics anyway, so this reads DCM's own JSON: findings
// for the gate and measures for the store, from one document. The decoder is
// partial on purpose, like the SARIF one, so a new field cannot break the parse.
package dcm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/verdict"
	"github.com/sherzing/assay/pkg/schema"
)

// Version is the DCM formatVersion this importer was verified against (DCM 1.39).
const Version = 13

// Tool namespaces every rule ID, so DCM rules cannot collide with another producer's.
const Tool = "dcm"

// Options controls an import.
type Options struct {
	// Root is the repository root; source is read from under it for spans and markers.
	Root string
	// Config carries pattern verdicts from .quality.yaml.
	Config verdict.Config
	// MetricsAsFindings also reports threshold breaches as findings. Off by
	// default: a complexity gate gets gamed, and the measures carry the same data.
	MetricsAsFindings bool
}

// Result is what one report yields. Repo, commit and timestamp are the caller's to stamp.
type Result struct {
	FormatVersion int
	Timestamp     time.Time
	Findings      []model.Finding
	// Measures: function, class and file scope, plus project roll-ups of the function metrics.
	Measures []schema.Measure
	// Functions carry per-function metrics in the Go scan's shape; cognitive stays 0.
	Functions []model.FuncMetrics
	Files     int // distinct files mentioned
	Funcs     int // distinct functions with metrics
}

type importer struct {
	opt   Options
	src   map[string][]string // path -> lines; present but nil when unreadable
	decls map[string][]decl
	files map[string]bool
	funcs map[string]bool
	fm    map[string]*model.FuncMetrics
	res   Result
}

// Import reads one DCM JSON report.
func Import(r io.Reader, opt Options) (*Result, error) {
	root, err := readRoot(r)
	if err != nil {
		return nil, err
	}
	im := newImporter(opt)
	if err := im.header(root); err != nil {
		return nil, err
	}
	if err := im.sections(root); err != nil {
		return nil, err
	}
	im.finish()
	return &im.res, nil
}

// ImportFile reads a report from disk.
func ImportFile(path string, opt Options) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Import(f, opt)
}

// Merge combines several reports into one, recomputing the project roll-ups over the union.
func Merge(rs ...*Result) *Result {
	if len(rs) == 1 {
		return rs[0]
	}
	out := &Result{}
	for _, r := range rs {
		out.FormatVersion = max(out.FormatVersion, r.FormatVersion)
		out.Findings = append(out.Findings, r.Findings...)
		out.Functions = append(out.Functions, r.Functions...)
		out.Files += r.Files
		out.Funcs += r.Funcs
		for _, m := range r.Measures {
			if m.Scope != schema.ScopeProject {
				out.Measures = append(out.Measures, m)
			}
		}
	}
	aggregate(out)
	return out
}

func readRoot(r io.Reader) (map[string]json.RawMessage, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("dcm: not a JSON object: %w", err)
	}
	return root, nil
}

func newImporter(opt Options) *importer {
	return &importer{
		opt: opt, src: map[string][]string{}, decls: map[string][]decl{},
		files: map[string]bool{}, funcs: map[string]bool{}, fm: map[string]*model.FuncMetrics{},
	}
}

// header reads the version and timestamp; no version means this is not a DCM report.
func (im *importer) header(root map[string]json.RawMessage) error {
	if raw, ok := root["formatVersion"]; ok {
		if err := json.Unmarshal(raw, &im.res.FormatVersion); err != nil {
			return fmt.Errorf("dcm: formatVersion: %w", err)
		}
	}
	if im.res.FormatVersion == 0 {
		return fmt.Errorf("dcm: no formatVersion in the root object; is this a DCM JSON report?")
	}
	if t, err := time.Parse("2006-01-02 15:04:05", str(root["timestamp"])); err == nil {
		im.res.Timestamp = t.UTC()
	}
	return nil
}

// sections maps a JSON key to the command name used in rule IDs; unknown keys are kebab-cased.
var sections = map[string]string{
	"analyzeResults":                    "analyze",
	"metricResults":                     "metrics",
	"unusedCodeResults":                 "unused-code",
	"unusedFilesResults":                "unused-files",
	"unusedL10nResults":                 "unused-l10n",
	"duplicationResults":                "code-duplication",
	"dependenciesResults":               "dependencies",
	"parametersResults":                 "parameters",
	"exportResults":                     "exports-completeness",
	"widgetResults":                     "widgets",
	"unnecessarilyPublicCodeResults":    "unnecessarily-public-code",
	"unnecessarilyMutableFieldsResults": "unnecessarily-mutable-fields",
}

// sections decodes every *Results array, metrics first so lint issues can be attributed.
func (im *importer) sections(root map[string]json.RawMessage) error {
	if raw, ok := root["metricResults"]; ok {
		files, err := readFiles(raw)
		if err != nil {
			return fmt.Errorf("dcm: metricResults: %w", err)
		}
		im.values(files)
	}
	keys := make([]string, 0, len(root))
	for k := range root {
		if strings.HasSuffix(k, "Results") && k != "metricResults" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		files, err := readFiles(root[k])
		if err != nil {
			return fmt.Errorf("dcm: %s: %w", k, err)
		}
		im.findings(sectionName(k), files)
	}
	return nil
}

// finish aggregates, numbers duplicates and sorts, so the output is deterministic.
func (im *importer) finish() {
	im.res.Files = len(im.files)
	im.res.Funcs = len(im.funcs)
	aggregate(&im.res)
	im.disambiguate()
	im.functions()
	sort.Slice(im.res.Findings, func(i, j int) bool {
		a, b := im.res.Findings[i], im.res.Findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Rule < b.Rule
	})
}
