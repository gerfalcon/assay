// Package schema defines the three record types every assay tool reads and writes.
//
// THE CONTRACT IS THE FORMAT, NOT THIS PACKAGE. Unix composability comes from a
// shared data format — `ls | grep | wc` works because of text, not because of
// interfaces. These Go types are one implementation of the JSON Schemas in
// ../../schemas/; a conforming tool can be written in any language and will
// compose with ours over a pipe.
//
// Everything is JSON Lines: one record per line, streamable, greppable, and
// readable without any of our binaries. That last property is deliberate — see
// the Faros episode in the platform survey for why not to make people depend on
// a vendor's binary, ours included.
package schema

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Version is the record schema version. Bump on any change to a field's meaning;
// a trend series is only useful if a record written in 2026 still parses in 2031.
const Version = 1

// Kind discriminates the three record types on a mixed stream.
type Kind string

const (
	KindFinding Kind = "finding"
	KindMeasure Kind = "measure"
	KindVerdict Kind = "verdict"
)

// Scope is the granularity a measurement applies to. One shape for every level
// means the tool that plots project trends and the tool that ranks file
// hotspots read the same stream.
type Scope string

const (
	ScopeProject  Scope = "project"
	ScopeModule   Scope = "module"
	ScopeFile     Scope = "file"
	ScopeFunction Scope = "function"
)

// Severity ranks a finding.
type Severity string

const (
	SevError Severity = "error"
	SevWarn  Severity = "warn"
	SevInfo  Severity = "info"
)

// Judgement is a human's decision about a finding.
//
// The three-way split is the point of the whole project. A single "tolerated"
// bucket conflates debt with noise:
//
//   - Accepted is real and should eventually be paid down.
//   - FalsePositive means the RULE is wrong, and feeds rule quality measurement.
//   - WontFix is real and deliberately not being fixed.
//
// Separating them is what turns a suppression list into a precision dataset.
type Judgement string

const (
	Accepted      Judgement = "accepted"
	FalsePositive Judgement = "false-positive"
	WontFix       Judgement = "wont-fix"
)

// Finding is one actionable violation. Every field exists to answer "what do I
// do about it" — Rule says which check, File/Line says where, Suggest says how.
type Finding struct {
	V           int       `json:"v"`
	Kind        Kind      `json:"kind"`
	Repo        string    `json:"repo,omitempty"`
	Commit      string    `json:"commit,omitempty"`
	TS          time.Time `json:"ts"`
	Tool        string    `json:"tool,omitempty"`
	Rule        string    `json:"rule"`
	Severity    Severity  `json:"severity"`
	File        string    `json:"file"`
	Line        int       `json:"line,omitempty"`
	Col         int       `json:"col,omitempty"`
	Symbol      string    `json:"symbol,omitempty"`
	Message     string    `json:"message"`
	Suggest     string    `json:"suggest,omitempty"`
	Fingerprint string    `json:"fingerprint"`
}

// Measure is one number at one point in time.
type Measure struct {
	V      int       `json:"v"`
	Kind   Kind      `json:"kind"`
	Repo   string    `json:"repo"`
	Commit string    `json:"commit,omitempty"`
	TS     time.Time `json:"ts"`
	Scope  Scope     `json:"scope"`
	Path   string    `json:"path,omitempty"`
	Metric string    `json:"metric"`
	Value  float64   `json:"value"`
}

// Verdict is a human judgement about a finding, identified by fingerprint.
//
// Stored as an append-only log rather than a mutable record: the audit trail
// comes free, last-write-wins needs no transactions, and "who decided this and
// why" survives the person leaving.
type Verdict struct {
	V           int       `json:"v"`
	Kind        Kind      `json:"kind"`
	Fingerprint string    `json:"fingerprint"`
	Rule        string    `json:"rule,omitempty"`
	Repo        string    `json:"repo,omitempty"`
	Verdict     Judgement `json:"verdict"`
	Reason      string    `json:"reason,omitempty"`
	By          string    `json:"by,omitempty"`
	TS          time.Time `json:"ts"`
}

// Record is a decoded line off a mixed stream. Exactly one field is non-nil.
type Record struct {
	Finding *Finding
	Measure *Measure
	Verdict *Verdict
}

type kindProbe struct {
	Kind Kind `json:"kind"`
	V    int  `json:"v"`
}

// Decode reads a JSONL stream and calls fn for each record.
//
// Unknown kinds and malformed lines are skipped rather than fatal: a long pipe
// should not die because one producer emitted something we do not understand
// yet. Errors are reported through onErr so a caller can still count them.
func Decode(r io.Reader, fn func(Record) error, onErr func(line int, err error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // findings can carry long messages
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var probe kindProbe
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			if onErr != nil {
				onErr(n, err)
			}
			continue
		}
		var rec Record
		var err error
		switch probe.Kind {
		case KindFinding:
			var f Finding
			if err = json.Unmarshal([]byte(line), &f); err == nil {
				rec.Finding = &f
			}
		case KindMeasure:
			var m Measure
			if err = json.Unmarshal([]byte(line), &m); err == nil {
				rec.Measure = &m
			}
		case KindVerdict:
			var v Verdict
			if err = json.Unmarshal([]byte(line), &v); err == nil {
				rec.Verdict = &v
			}
		default:
			continue // a kind from a newer tool; ignore rather than fail
		}
		if err != nil {
			if onErr != nil {
				onErr(n, err)
			}
			continue
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Encoder writes JSONL.
type Encoder struct {
	w   *bufio.Writer
	enc *json.Encoder
}

func NewEncoder(w io.Writer) *Encoder {
	bw := bufio.NewWriter(w)
	return &Encoder{w: bw, enc: json.NewEncoder(bw)}
}

// Write emits one record. Kind and V are set here so no caller can forget them
// and produce a line nothing downstream can route.
func (e *Encoder) Write(v any) error {
	switch t := v.(type) {
	case *Finding:
		t.V, t.Kind = Version, KindFinding
	case *Measure:
		t.V, t.Kind = Version, KindMeasure
	case *Verdict:
		t.V, t.Kind = Version, KindVerdict
	default:
		return fmt.Errorf("schema: cannot encode %T", v)
	}
	return e.enc.Encode(v)
}

func (e *Encoder) Flush() error { return e.w.Flush() }
