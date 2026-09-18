// Package baseline implements the ratchet: record what is wrong today, then
// fail only on what gets worse.
//
// This is the feature the tool is named for. Big-bang conformance does not land
// on an existing codebase — you turn on a rule, get four thousand violations,
// and the team switches it off that afternoon. A ratchet inverts that: existing
// violations are recorded and tolerated, new ones fail the build. The codebase
// can only improve or hold, never regress, and nobody has to stop and fix four
// thousand things first.
//
// Generalised from ArchUnit's FreezingArchRule, including its key safety
// property: the baseline must not be silently regenerable, or "fix the failure"
// becomes "rewrite the baseline" and the gate quietly stops meaning anything.
package baseline

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/sherzing/ratchet/internal/model"
)

// FileVersion guards against reading a baseline written by an incompatible build.
const FileVersion = "1"

// Entry is one tolerated finding. Rule/File/Func are stored for human
// readability in review — the fingerprint is what actually matches.
type Entry struct {
	Rule string `json:"rule"`
	File string `json:"file"`
	Func string `json:"func,omitempty"`
	Note string `json:"note,omitempty"`
}

// Baseline is the recorded state of the codebase at acceptance time.
type Baseline struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	// Tolerated maps fingerprint to entry.
	Tolerated map[string]Entry `json:"tolerated"`
	// Caps are aggregate ceilings recorded at baseline time. These guard the
	// gap the fingerprint set cannot: a brand-new function with complexity 90
	// has a new fingerprint for its findings, but if it triggers no rule at all
	// it would otherwise pass unnoticed.
	Caps Caps `json:"caps"`
}

// Caps are the aggregate guards. Deliberately few. Gating on an aggregate score
// invites a probabilistic optimiser to game it — split one big function into
// six bad small ones and the mean improves — so these are maxima on the tail,
// not averages, and they are advisory unless --strict-caps is passed.
type Caps struct {
	MaxCyclomatic int `json:"maxCyclomatic"`
	MaxCognitive  int `json:"maxCognitive"`
	MaxNesting    int `json:"maxNesting"`
}

// From builds a baseline from a scan.
func From(rep *model.Report, commit string) *Baseline {
	b := &Baseline{
		Version:   FileVersion,
		Commit:    commit,
		Tolerated: make(map[string]Entry, len(rep.Findings)),
		Caps: Caps{
			MaxCyclomatic: rep.Summary.Cyclomatic.Max,
			MaxCognitive:  rep.Summary.Cognitive.Max,
			MaxNesting:    rep.Summary.MaxNesting.Max,
		},
	}
	for _, f := range rep.Findings {
		b.Tolerated[f.Fingerprint] = Entry{Rule: f.Rule, File: f.File, Func: f.Func}
	}
	return b
}

// Load reads a baseline from disk.
func Load(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	if b.Version != FileVersion {
		return nil, fmt.Errorf("baseline %s has version %q, this build expects %q",
			path, b.Version, FileVersion)
	}
	if b.Tolerated == nil {
		b.Tolerated = map[string]Entry{}
	}
	return &b, nil
}

// Save writes a baseline. Indented and key-sorted on purpose: this file is
// checked into git, and a rewrite must be legible in review.
func (b *Baseline) Save(path string) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Result is the outcome of a check.
type Result struct {
	New      []model.Finding `json:"new"`
	Fixed    []Entry         `json:"fixed"`
	Existing int             `json:"existing"`
	CapBreak []string        `json:"capBreaches"`
}

// Regressed reports whether the gate should fail the build.
func (r Result) Regressed() bool { return len(r.New) > 0 || len(r.CapBreak) > 0 }

// Check compares a fresh scan against the baseline.
//
// Only three outcomes matter: findings not in the baseline (new — these fail),
// baseline entries no longer present (fixed — these are the good news, and
// reporting them is what makes the ratchet feel like progress rather than
// policing), and everything else (existing — tolerated silently).
func (b *Baseline) Check(rep *model.Report, strictCaps bool) Result {
	var res Result
	seen := map[string]bool{}

	for _, f := range rep.Findings {
		seen[f.Fingerprint] = true
		if _, ok := b.Tolerated[f.Fingerprint]; ok {
			res.Existing++
			continue
		}
		res.New = append(res.New, f)
	}

	for fp, e := range b.Tolerated {
		if !seen[fp] {
			res.Fixed = append(res.Fixed, e)
		}
	}
	sort.Slice(res.Fixed, func(i, j int) bool {
		if res.Fixed[i].File != res.Fixed[j].File {
			return res.Fixed[i].File < res.Fixed[j].File
		}
		return res.Fixed[i].Rule < res.Fixed[j].Rule
	})

	if strictCaps {
		if v := rep.Summary.Cyclomatic.Max; v > b.Caps.MaxCyclomatic {
			res.CapBreak = append(res.CapBreak,
				fmt.Sprintf("max cyclomatic %d exceeds baseline %d", v, b.Caps.MaxCyclomatic))
		}
		if v := rep.Summary.Cognitive.Max; v > b.Caps.MaxCognitive {
			res.CapBreak = append(res.CapBreak,
				fmt.Sprintf("max cognitive %d exceeds baseline %d", v, b.Caps.MaxCognitive))
		}
		if v := rep.Summary.MaxNesting.Max; v > b.Caps.MaxNesting {
			res.CapBreak = append(res.CapBreak,
				fmt.Sprintf("max nesting %d exceeds baseline %d", v, b.Caps.MaxNesting))
		}
	}
	return res
}

// Tighten removes entries that no longer reproduce and lowers the caps.
//
// Behind an explicit flag rather than automatic. Auto-tightening on a shared
// branch is hostile: your colleague's merge silently ratchets the bar, and the
// next person to rebase fails a check for code they never touched.
func (b *Baseline) Tighten(rep *model.Report) int {
	seen := map[string]bool{}
	for _, f := range rep.Findings {
		seen[f.Fingerprint] = true
	}
	removed := 0
	for fp := range b.Tolerated {
		if !seen[fp] {
			delete(b.Tolerated, fp)
			removed++
		}
	}
	if v := rep.Summary.Cyclomatic.Max; v < b.Caps.MaxCyclomatic {
		b.Caps.MaxCyclomatic = v
	}
	if v := rep.Summary.Cognitive.Max; v < b.Caps.MaxCognitive {
		b.Caps.MaxCognitive = v
	}
	if v := rep.Summary.MaxNesting.Max; v < b.Caps.MaxNesting {
		b.Caps.MaxNesting = v
	}
	return removed
}
