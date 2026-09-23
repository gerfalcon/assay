// Package dcm ingests DCM (dcm.dev) JSON reports, so the ratchet, the history
// and the tickets work on Dart and Flutter code. DCM has no SARIF reporter, and
// SARIF would drop the metrics anyway, so this reads DCM's own JSON: findings
// for the gate and measures for the store, from one document. The decoder is
// partial on purpose, like the SARIF one, so a new field cannot break the parse.
package dcm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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

// Sniff reports whether data is a DCM report: its root carries formatVersion, SARIF's does not.
func Sniff(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.Contains(head, []byte(`"formatVersion"`))
}

type fileResult struct {
	Path   string          `json:"path"`
	Issues json.RawMessage `json:"issues"`
}

type issue struct {
	ID              string          `json:"id"`
	Message         string          `json:"message"`
	Severity        string          `json:"severity"`
	Location        location        `json:"location"`
	DeclarationName string          `json:"declarationName"`
	DeclarationType string          `json:"declarationType"`
	Level           string          `json:"level"`
	Value           json.RawMessage `json:"value"`
	Duplications    []duplicate     `json:"duplications"`
}

// duplicate is one other copy of a duplicated declaration.
type duplicate struct {
	DeclarationName string   `json:"declarationName"`
	DeclarationType string   `json:"declarationType"`
	Location        location `json:"location"`
	RelativePath    string   `json:"relativePath"`
}

type location struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn"`
	EndLine     int `json:"endLine"`
	EndColumn   int `json:"endColumn"`
}

type fileIssues struct {
	path   string
	issues []issue
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

// decl is a declaration range from the metrics section, used to attribute lint issues to their function.
type decl struct {
	name       string
	start, end int
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

// ImportFile reads a report from disk.
func ImportFile(path string, opt Options) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Import(f, opt)
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

func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// num reads a metric value; an unreadable one is skipped, not fatal.
func num(raw json.RawMessage) (float64, bool) {
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	if s := str(raw); s != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func readFiles(raw json.RawMessage) ([]fileIssues, error) {
	var frs []fileResult
	if err := json.Unmarshal(raw, &frs); err != nil {
		return nil, err
	}
	out := make([]fileIssues, 0, len(frs))
	for _, fr := range frs {
		issues, err := readIssues(fr.Issues)
		if err != nil {
			return nil, fmt.Errorf("%s: issues: %w", fr.Path, err)
		}
		out = append(out, fileIssues{path: filepath.ToSlash(fr.Path), issues: issues})
	}
	return out, nil
}

// readIssues accepts an array or a single object; the docs show both.
func readIssues(raw json.RawMessage) ([]issue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var many []issue
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one issue
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, err
	}
	return []issue{one}, nil
}

func sectionName(key string) string {
	if s, ok := sections[key]; ok {
		return s
	}
	var b strings.Builder
	for i, r := range strings.TrimSuffix(key, "Results") {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------- metrics ----------

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

// level normalises the metric level: the docs say very-high, the tool says very high.
func level(s string) string { return kebab(s) }

// kebab makes a DCM label safe inside a rule ID.
func kebab(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "-"), "_", "-")
}

func breached(l string) bool {
	l = level(l)
	return l == "high" || l == "very-high"
}

// breach is a threshold violation as a finding.
func (im *importer) breach(path string, is issue) {
	im.add(model.Finding{
		Rule: Tool + ":metrics:" + is.ID, Severity: levelSeverity(is.Level), File: path,
		Line: is.Location.StartLine, Col: is.Location.StartColumn,
		Func: is.DeclarationName, Message: is.Message,
		Suggest: "split it, or raise the threshold in analysis_options.yaml on purpose",
	}, "metric "+is.DeclarationName)
}

// index records a declaration's range once.
func (im *importer) index(path, name string, loc location) {
	end := loc.EndLine
	if end < loc.StartLine {
		end = loc.StartLine
	}
	for _, d := range im.decls[path] {
		if d.name == name && d.start == loc.StartLine {
			return
		}
	}
	im.decls[path] = append(im.decls[path], decl{name: name, start: loc.StartLine, end: end})
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

func levelSeverity(l string) model.Severity {
	if level(l) == "very-high" {
		return model.Warn
	}
	return model.Info
}

// ---------- findings ----------

func (im *importer) findings(section string, files []fileIssues) {
	for _, f := range files {
		im.files[f.path] = true
		for _, is := range f.issues {
			im.finding(section, f.path, is)
		}
	}
}

// finding maps one issue: severity from the section or the issue, function from the metrics index.
func (im *importer) finding(section, path string, is issue) {
	sev := defaultSeverity(section)
	if section == "analyze" {
		sev = severity(is.Severity)
	}
	fn := is.DeclarationName
	if fn == "" {
		fn = im.enclosing(path, is.Location.StartLine)
	}
	im.add(model.Finding{
		Rule: ruleID(section, is), Severity: sev, File: path,
		Line: is.Location.StartLine, Col: is.Location.StartColumn,
		Func: fn, Message: message(section, is),
	}, im.basis(section, path, is))
}

// add fingerprints, judges and records one finding. basis identifies the instance beyond
// file, rule and function, never by line number.
func (im *importer) add(f model.Finding, basis string) {
	f.Fingerprint = model.Fingerprint(f.File, f.Rule, f.Func, basis)
	im.judge(&f)
	im.res.Findings = append(im.res.Findings, f)
}

// ruleID: dcm:<rule> for lint, dcm:<command>[:<kind>] otherwise. A generic -issue id adds nothing.
func ruleID(section string, is issue) string {
	if section == "analyze" {
		if is.ID == "" {
			return Tool + ":unknown"
		}
		return Tool + ":" + is.ID
	}
	if section == "unused-code" && is.DeclarationType != "" {
		return Tool + ":unused-code:" + kebab(is.DeclarationType)
	}
	if is.ID != "" && !strings.HasSuffix(is.ID, "-issue") {
		return Tool + ":" + section + ":" + is.ID
	}
	return Tool + ":" + section
}

// defaultSeverity applies to sections whose issues carry none.
func defaultSeverity(section string) model.Severity {
	switch section {
	case "unused-code", "unused-files", "unused-l10n", "code-duplication", "dependencies", "exports-completeness":
		return model.Warn
	}
	return model.Info
}

func severity(s string) model.Severity {
	switch strings.ToLower(s) {
	case "error":
		return model.Error
	case "warning":
		return model.Warn
	}
	return model.Info
}

func message(section string, is issue) string {
	msg := is.Message
	if msg == "" {
		msg = strings.TrimSpace(section + " " + is.DeclarationType + " " + is.DeclarationName)
	}
	if len(is.Duplications) > 0 {
		msg += " — also " + duplicatesText(is.Duplications)
	}
	return msg
}

// duplicatesText names the other copies, so a ticket says what to merge.
func duplicatesText(ds []duplicate) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		where := d.RelativePath
		if d.Location.StartLine > 0 {
			where += ":" + strconv.Itoa(d.Location.StartLine)
		}
		if d.DeclarationName != "" {
			where += " (" + d.DeclarationName + ")"
		}
		parts = append(parts, where)
	}
	return strings.Join(parts, ", ")
}

// basis picks what identifies an instance: a lint issue's span, a declaration's name, or the file.
func (im *importer) basis(section, path string, is issue) string {
	switch section {
	case "unused-files":
		return ""
	case "analyze":
		if wholeFile(is.Location) {
			return "" // the file is the finding; its first line is not
		}
		if line, ok := im.span(path, is.Location); ok {
			return line
		}
	}
	if is.DeclarationName != "" {
		return is.DeclarationType + " " + is.DeclarationName
	}
	return is.Message
}

// wholeFile recognises a region covering the file from line 1, like avoid-long-files.
func wholeFile(loc location) bool {
	return loc.StartLine <= 1 && loc.EndLine > loc.StartLine
}

// span returns the first line of the reported region from its start column.
func (im *importer) span(path string, loc location) (string, bool) {
	lines, ok := im.lines(path)
	if !ok || loc.StartLine < 1 || loc.StartLine > len(lines) {
		return "", false
	}
	line := []rune(lines[loc.StartLine-1])
	start := loc.StartColumn - 1
	if start < 0 || start > len(line) {
		start = 0
	}
	end := len(line)
	if loc.EndLine == loc.StartLine && loc.EndColumn-1 > start && loc.EndColumn-1 <= len(line) {
		end = loc.EndColumn - 1
	}
	s := strings.TrimSpace(string(line[start:end]))
	if s == "" {
		return "", false
	}
	return s, true
}

// enclosing names the innermost declaration covering the line.
func (im *importer) enclosing(path string, line int) string {
	best := ""
	size := -1
	for _, d := range im.decls[path] {
		if line < d.start || line > d.end {
			continue
		}
		if size < 0 || d.end-d.start < size {
			best, size = d.name, d.end-d.start
		}
	}
	return best
}

func (im *importer) lines(path string) ([]string, bool) {
	if l, ok := im.src[path]; ok {
		return l, l != nil
	}
	data, err := os.ReadFile(filepath.Join(im.opt.Root, path))
	if err != nil {
		im.src[path] = nil // unreadable: identity falls back to the message
		return nil, false
	}
	l := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	im.src[path] = l
	return l, true
}

// ---------- verdicts ----------

// judge resolves a verdict like the Go scan: marker first, then config. It never bypasses the gate.
func (im *importer) judge(f *model.Finding) {
	v, ok := im.marker(f.File, f.Line)
	if !ok {
		v, ok = im.opt.Config.Match(f.Rule, f.File)
	}
	if !ok {
		return
	}
	f.Verdict, f.VerdictWhy, f.VerdictFrom = string(v.Verdict), v.Reason, v.Source
	if !v.Until.IsZero() {
		f.VerdictUntil = v.Until.Format("2006-01-02")
	}
}

// marker looks for a quality: comment on the line or the line above. Text-based: no Dart parser.
func (im *importer) marker(path string, line int) (verdict.V, bool) {
	lines, ok := im.lines(path)
	if !ok || line < 1 || line > len(lines) {
		return verdict.V{}, false
	}
	for _, n := range []int{line, line - 1} {
		if n < 1 {
			continue
		}
		if v, ok := verdict.Parse(lines[n-1]); ok {
			v.Line = n
			return v, true
		}
	}
	return verdict.V{}, false
}

// disambiguate numbers identical findings in source order, so the gate tracks their count
// without line numbers. On a real app 13% of findings collided before this.
func (im *importer) disambiguate() {
	byFP := map[string][]int{}
	for i, f := range im.res.Findings {
		byFP[f.Fingerprint] = append(byFP[f.Fingerprint], i)
	}
	for fp, idx := range byFP {
		if len(idx) < 2 {
			continue
		}
		fs := im.res.Findings
		sort.Slice(idx, func(a, b int) bool {
			if fs[idx[a]].Line != fs[idx[b]].Line {
				return fs[idx[a]].Line < fs[idx[b]].Line
			}
			return fs[idx[a]].Col < fs[idx[b]].Col
		})
		for k, i := range idx[1:] {
			f := &fs[i]
			f.Fingerprint = model.Fingerprint(f.File, f.Rule, f.Func, fp+"#"+strconv.Itoa(k+2))
		}
	}
}

// ---------- roll-ups ----------

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
