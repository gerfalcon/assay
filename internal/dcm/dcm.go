// Package dcm ingests DCM (dcm.dev) JSON reports, so the history and lens read
// Dart and Flutter metrics. DCM has no SARIF reporter, and SARIF would drop the
// metrics anyway, so this reads DCM's own JSON. The decoder is partial on
// purpose, like the SARIF one, so a new field cannot break the parse.
package dcm

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/pkg/schema"
)

// Version is the DCM formatVersion this importer was verified against (DCM 1.39).
const Version = 13

// Options controls an import.
type Options struct {
	// Root is the repository root the report's paths are relative to.
	Root string
}

// Result is what one report yields. Repo, commit and timestamp are the caller's to stamp.
type Result struct {
	FormatVersion int
	Timestamp     time.Time
	// Measures: function, class and file scope, plus project roll-ups of the function metrics.
	Measures []schema.Measure
	// Functions carry per-function metrics in the Go scan's shape; cognitive stays 0.
	Functions []model.FuncMetrics
	Files     int // distinct files mentioned
	Funcs     int // distinct functions with metrics
}

type importer struct {
	opt   Options
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
		opt: opt, files: map[string]bool{}, funcs: map[string]bool{}, fm: map[string]*model.FuncMetrics{},
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

// sections decodes the metrics; the other *Results arrays are findings, which
// this half of the importer does not read.
func (im *importer) sections(root map[string]json.RawMessage) error {
	raw, ok := root["metricResults"]
	if !ok {
		return nil
	}
	files, err := readFiles(raw)
	if err != nil {
		return fmt.Errorf("dcm: metricResults: %w", err)
	}
	im.values(files)
	return nil
}

// finish aggregates and lists the functions in a deterministic order.
func (im *importer) finish() {
	im.res.Files = len(im.files)
	im.res.Funcs = len(im.funcs)
	aggregate(&im.res)
	im.functions()
}
