// Package dcm ingests DCM (dcm.dev) JSON reports, so the history and lens read
// Dart and Flutter metrics. DCM has no SARIF reporter, and SARIF would drop the
// metrics anyway, so this reads DCM's own JSON.
//
// This is the contract: the types, the functions, the fixture under testdata
// and the tests that pin what the importer must make of it. The implementation
// follows in the next change; until then Import reports ErrNotImplemented and
// the tests skip.
package dcm

import (
	"bytes"
	"errors"
	"io"
	"time"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/pkg/schema"
)

// Version is the DCM formatVersion this importer is written against (DCM 1.39).
const Version = 13

// ErrNotImplemented is what Import returns until the importer lands.
var ErrNotImplemented = errors.New("dcm: importer not implemented yet")

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

// Sniff reports whether data is a DCM report: its root carries formatVersion, SARIF's does not.
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

// Merge combines several reports into one, recomputing the project roll-ups over the union.
func Merge(rs ...*Result) *Result {
	if len(rs) == 1 {
		return rs[0]
	}
	return &Result{}
}
