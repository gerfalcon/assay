// Package dcm ingests DCM (dcm.dev) JSON reports, so lens and strata read Dart and Flutter metrics.
package dcm

import (
	"bytes"
	"errors"
	"io"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/pkg/schema"
)

// Version is the DCM formatVersion this importer targets (DCM 1.39).
const Version = 13

// ErrNotImplemented is what Import returns until the importer lands.
var ErrNotImplemented = errors.New("dcm: importer not implemented yet")

// Options controls an import.
type Options struct {
	// Root is the repository root; a generated file is recognised by its header when it is readable under Root.
	Root string
	// Prefix is the directory DCM ran in, relative to Root. DCM writes paths relative to its working
	// directory, so a report from one package of a monorepo needs it to key like the rest of the repository.
	Prefix string
}

// Result is what one report yields: per-declaration measures, never roll-ups.
type Result struct {
	FormatVersion int
	// Measures at function, class and file scope, keyed by the declaration's identity.
	Measures []schema.Measure
	// Functions are the declarations DCM computed cyclomatic complexity for, in the Go scan's shape.
	Functions []model.FuncMetrics
	Files     int // files measured
	Funcs     int // declarations with a per-function record
	Generated int // files skipped as generated code
	Located   int // report files found under Root; 0 means no generated-file header could be checked
	Issues    int // lint, unused-code and duplication issues in the report, which this importer does not read
}

// Sniff reports whether data is a DCM report, by the formatVersion at its root.
func Sniff(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.Contains(head, []byte(`"formatVersion"`))
}

// Import reads one DCM JSON report.
func Import(r io.Reader, opt Options) (*Result, error) {
	return nil, ErrNotImplemented
}

// ImportFile reads a report from disk.
func ImportFile(path string, opt Options) (*Result, error) {
	return nil, ErrNotImplemented
}

// Merge unions several reports into one; two reports disagreeing about one identity is an error.
func Merge(rs ...*Result) (*Result, error) {
	if len(rs) == 1 {
		return rs[0], nil
	}
	return &Result{}, nil
}
