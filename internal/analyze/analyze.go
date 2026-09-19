// Package analyze walks Go source with go/ast and produces the measurement
// record. It deliberately parses without type information: no go/types, no
// package loading, no build constraints resolution. That costs some precision
// (see looksLikeErr) and buys the ability to replay over thousands of historical
// commits, many of which will not even compile with the current toolchain.
package analyze

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/verdict"
)

func schemaJudgement(s string) schemaJ { return schemaJ(s) }

// Options controls a scan.
type Options struct {
	// Enabled, if non-nil, restricts which rules run.
	Enabled map[string]bool
	// IncludeTests analyses _test.go files. Off by default: test code has
	// legitimately different norms and including it swamps the signal.
	IncludeTests bool
	// IncludeVendor analyses vendor/ and generated code. Off by default:
	// vendored code is not ours to fix and changes en masse, which would
	// dominate any trend line.
	IncludeVendor bool
	// SkipDirs are additional directory names to skip.
	SkipDirs []string
	// Detail emits per-function records. Off for history replay, where only
	// the summary is kept.
	Detail bool
	// Config carries pattern-matched verdicts from project config.
	Config verdict.Config
}

// LoadConfig reads project-level verdicts.
//
// Neutral filename on purpose, same reasoning as the comment marker: another
// tool should be able to read this file and honour the same decisions.
// Tries .quality.yaml, .quality.yml, then .quality.json.
func LoadConfig(root string) (verdict.Config, error) {
	var cfg verdict.Config
	for _, name := range []string{".quality.yaml", ".quality.yml", ".quality.json"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		if strings.HasSuffix(name, ".json") {
			if err := json.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("%s: %w", name, err)
			}
		} else {
			parsed, err := parseMinimalYAML(data)
			if err != nil {
				return cfg, fmt.Errorf("%s: %w", name, err)
			}
			cfg = parsed
		}
		if errs := cfg.Validate(); len(errs) > 0 {
			return cfg, fmt.Errorf("%s: %v", name, errs[0])
		}
		return cfg, nil
	}
	return cfg, nil
}

// parseMinimalYAML reads the small subset this config needs.
//
// Hand-rolled to keep the dependency count at zero. That is a real constraint
// here: a quality tool that drags in a YAML library is one more thing to audit,
// and the schema is six keys. If it ever needs anchors or nesting, take the
// dependency rather than growing this.
func parseMinimalYAML(data []byte) (verdict.Config, error) {
	var cfg verdict.Config
	var cur *verdict.Rule
	inList := false

	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(line, "verdicts:") {
			inList = true
			continue
		}
		if !inList {
			continue
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "- ") {
			cfg.Verdicts = append(cfg.Verdicts, verdict.Rule{})
			cur = &cfg.Verdicts[len(cfg.Verdicts)-1]
			t = strings.TrimSpace(t[2:])
			if t == "" {
				continue
			}
		}
		if cur == nil {
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			return cfg, fmt.Errorf("line %d: expected key: value, got %q", i+1, t)
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "rule":
			cur.Rule = v
		case "path":
			cur.Path = v
		case "verdict":
			cur.Verdict = schemaJudgement(v)
		case "reason":
			cur.Reason = v
		case "until":
			cur.Until = v
		default:
			return cfg, fmt.Errorf("line %d: unknown key %q", i+1, strings.TrimSpace(k))
		}
	}
	return cfg, nil
}

var defaultSkip = map[string]bool{
	".git": true, "vendor": true, "node_modules": true,
	"testdata": true, ".idea": true, ".vscode": true,
}

// Scan analyses every Go file under root.
//
// Orchestration only: decide which files to visit, hand each to scanFile, sort.
// The per-file work lives in scanFile because assay flagged this function at
// cognitive 41 — the worst in its own codebase — and it was right.
func Scan(root string, opt Options) (*model.Report, error) {
	rep := &model.Report{Root: root, Findings: []model.Finding{}}
	fset := token.NewFileSet()
	skip := skipSet(opt)
	files := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable paths are skipped, not fatal
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !wanted(path, opt) {
			return nil
		}
		fnd, fns, ok := scanFile(fset, root, path, opt)
		if !ok {
			return nil
		}
		files++
		rep.Findings = append(rep.Findings, fnd...)
		rep.Funcs = append(rep.Funcs, fns...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sortReport(rep)
	rep.Summarise(files)
	if !opt.Detail {
		rep.Funcs = nil
	}
	return rep, nil
}

// skipSet builds the directory-name blocklist for this scan.
func skipSet(opt Options) map[string]bool {
	skip := map[string]bool{}
	for k, v := range defaultSkip {
		skip[k] = v
	}
	if opt.IncludeVendor {
		delete(skip, "vendor")
	}
	for _, d := range opt.SkipDirs {
		skip[d] = true
	}
	return skip
}

// wanted reports whether a path is a Go file we should measure.
func wanted(path string, opt Options) bool {
	if !strings.HasSuffix(path, ".go") {
		return false
	}
	return opt.IncludeTests || !strings.HasSuffix(path, "_test.go")
}

// scanFile measures one file. ok is false when the file should not count
// towards the file total — unreadable, generated, or unparseable.
func scanFile(fset *token.FileSet, root, path string, opt Options) ([]model.Finding, []model.FuncMetrics, bool) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, false
	}
	// Generated files are machine output. Measuring them tells us about a
	// generator, not about anyone's design decisions.
	if isGenerated(src) {
		return nil, nil, false
	}
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		// Not fatal: during history replay we meet plenty of broken
		// intermediate states, and one bad commit should not abort the run.
		return nil, nil, false
	}

	rel, rerr := filepath.Rel(root, path)
	if rerr != nil {
		rel = path
	}

	p := &smellPass{
		fset: fset, file: f, relPath: rel, src: src,
		isTest:  strings.HasSuffix(path, "_test.go"),
		isMain:  f.Name != nil && f.Name.Name == "main",
		enabled: opt.Enabled,
		config:  opt.Config,
	}
	return p.run(), funcMetrics(fset, f, rel), true
}

// funcMetrics extracts the per-function record for every function with a body.
func funcMetrics(fset *token.FileSet, f *ast.File, rel string) []model.FuncMetrics {
	var out []model.FuncMetrics
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		fm := model.FuncMetrics{
			Name:       fd.Name.Name,
			File:       rel,
			Line:       fset.Position(fd.Pos()).Line,
			Exported:   fd.Name.IsExported(),
			Cyclomatic: cyclomatic(fd),
			Cognitive:  cognitive(fd),
			MaxNesting: maxNesting(fd),
			Statements: countStatements(fd.Body),
			Params:     fieldCount(fd.Type.Params),
			Results:    fieldCount(fd.Type.Results),
		}
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			fm.Recv = recvTypeName(fd.Recv.List[0].Type)
		}
		out = append(out, fm)
	}
	return out
}

// sortReport imposes a deterministic order. Without it the JSON churns between
// runs on filesystem iteration order alone, making every diff unreadable and
// every baseline comparison noisy.
func sortReport(rep *model.Report) {
	sort.Slice(rep.Funcs, func(i, j int) bool {
		if rep.Funcs[i].File != rep.Funcs[j].File {
			return rep.Funcs[i].File < rep.Funcs[j].File
		}
		return rep.Funcs[i].Line < rep.Funcs[j].Line
	})
	sort.Slice(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Rule < b.Rule
	})
}

// isGenerated applies the convention from https://go.dev/s/generatedcode.
func isGenerated(src []byte) bool {
	const marker = "// Code generated "
	const suffix = " DO NOT EDIT."
	head := src
	if len(head) > 4096 {
		head = head[:4096]
	}
	for _, line := range strings.Split(string(head), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, marker) && strings.HasSuffix(line, suffix) {
			return true
		}
	}
	return false
}
