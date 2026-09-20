// Package learn derives candidate rules from codebases you already trust.
//
// The premise: if a team you consider good never does X, that is evidence X is
// worth flagging. If they do X constantly, it is evidence X is not a defect
// however many style guides say otherwise.
//
// THIS PRODUCES CANDIDATES, NOT RULES. A codebase can consistently do something
// bad, so a human still has to say which absences are deliberate. What the
// method reliably does is narrow the field and — more valuably — tell you which
// popular rules to NOT ship.
//
// Worked example from the two Go services this was built against: `naked return`
// appears on every Go style list and both codebases use it 3.6 and 4.4 times per
// thousand lines. Shipping it would have produced thousands of findings nobody
// agrees are defects. That rejection is worth more than most of the acceptances.
package learn

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Verdict classifies a candidate against the corpus.
type Verdict string

const (
	// Held — near-absent everywhere. The teams observe this convention.
	Held Verdict = "held"
	// NotHeld — common everywhere. They do not observe it; shipping it as a
	// rule would generate noise at scale.
	NotHeld Verdict = "not-held"
	// Divergent — one codebase avoids it, another does not. A matter of taste,
	// age or local context rather than a shared defect. Org rule at most.
	Divergent Verdict = "divergent"
)

// Thresholds for classification, tuned against real measurements rather than
// chosen for roundness. `panic` in library code sat at 0.11 and 0.05 per kLOC in
// two codebases that clearly avoid it, so the held line has to sit above that.
type Thresholds struct {
	HeldBelow      float64 // max rate under which a pattern counts as avoided
	DivergentRatio float64 // max/min above which codebases disagree
}

func DefaultThresholds() Thresholds {
	return Thresholds{HeldBelow: 0.15, DivergentRatio: 5}
}

// Candidate is one pattern worth probing.
//
// The catalogue is data rather than code so adding a candidate is one entry, and
// so a team can extend it for their own stack without touching the walker.
type Candidate struct {
	ID       string
	Why      string // what is wrong with it, for the generated rule's message
	Fix      string // what to do instead
	Severity string
	Pattern  *regexp.Regexp
	Semgrep  string // the pattern for the emitted rule, when it differs
	Exclude  *regexp.Regexp
}

// Result is one candidate measured across the corpus.
type Result struct {
	Candidate Candidate
	Rates     map[string]float64 // repo -> per kLOC
	Counts    map[string]int
	Verdict   Verdict
	Max, Min  float64
}

// Catalogue is the built-in probe set for Go.
//
// Deliberately includes patterns expected to FAIL. A probe set containing only
// things that turn out to be held would prove nothing — the rejections are how
// you know the method discriminates.
func Catalogue() []Candidate {
	must := regexp.MustCompile
	return []Candidate{
		{ID: "no-error-string-compare",
			Why: "comparing err.Error() to a string breaks when anyone rewords the message, and cannot see wrapped errors",
			Fix: "use errors.Is for sentinels or errors.As for types", Severity: "ERROR",
			Pattern: must(`\.Error\(\) *[=!]=|strings\.(Contains|HasPrefix)\([a-zA-Z_]*[eE]rr\.Error\(\)`),
			Semgrep: `$ERR.Error() == "..."`},
		{ID: "no-context-todo",
			Why: "context.TODO() is a placeholder the author meant to replace",
			Fix: "thread the caller's context through, or use context.Background() and say why", Severity: "WARNING",
			Pattern: must(`context\.TODO\(\)`), Semgrep: `context.TODO()`},
		{ID: "no-sleep-in-production",
			Why: "time.Sleep outside tests is usually a race being papered over or a retry without backoff",
			Fix: "use a ticker, a backoff, or wait on the thing you actually need", Severity: "WARNING",
			Pattern: must(`time\.Sleep\(`), Semgrep: `time.Sleep(...)`},
		{ID: "no-init-function",
			Why: "init() runs before main in an order nobody controls, making startup failures hard to trace",
			Fix: "construct explicitly from main", Severity: "WARNING",
			Pattern: must(`^func init\(\)`), Semgrep: "func init() { ... }"},
		{ID: "no-panic-in-library",
			Why: "panic() in library code takes the decision away from the caller",
			Fix: "return an error", Severity: "WARNING",
			Pattern: must(`^\s*panic\(`), Exclude: must(`func (M|m)ust`), Semgrep: `panic(...)`},
		{ID: "no-discarded-error",
			Why: "assigning an error to _ acknowledges a failure and then drops it",
			Fix: "handle it, or document why it is safe to ignore", Severity: "ERROR",
			Pattern: must(`_ = [a-zA-Z_]*[eE]rr\b`), Semgrep: `_ = $ERR`},
		{ID: "no-fmt-print-in-library",
			Why: "fmt.Print writes to stdout with no level, no context and no way to switch it off",
			Fix: "use the structured logger", Severity: "WARNING",
			Pattern: must(`fmt\.Print(f|ln)?\(`), Semgrep: `fmt.Print...(...)`},
		{ID: "wrap-errors-with-w",
			Why: "%v on an error flattens it, so errors.Is and errors.As stop working up the stack",
			Fix: "use %w", Severity: "WARNING",
			Pattern: must(`Errorf\([^)]*%v[^)]*err`), Semgrep: `fmt.Errorf("...%v...", $ERR)`},
		{ID: "prefer-any-over-interface",
			Why: "interface{} predates the any alias and reads worse",
			Fix: "use any", Severity: "INFO",
			Pattern: must(`interface\{\}`), Semgrep: `interface{}`},
		// The next four are expected to fail, and that is the point.
		{ID: "no-naked-return",
			Why: "a naked return hides what is being returned",
			Fix: "return the values explicitly", Severity: "INFO",
			Pattern: must(`^\s+return$`), Semgrep: "return"},
		{ID: "no-os-exit-outside-main",
			Why: "os.Exit skips deferred cleanup",
			Fix: "return an error to main", Severity: "WARNING",
			Pattern: must(`os\.Exit\(`), Semgrep: `os.Exit(...)`},
		{ID: "no-global-mutable-state",
			Why: "package-level mutable state makes tests order-dependent",
			Fix: "pass it explicitly", Severity: "INFO",
			Pattern: must(`^var [a-z][A-Za-z]* *=`), Semgrep: `var $X = ...`},
		{ID: "no-bare-goroutine",
			Why: "a goroutine with no recover takes the process down on panic",
			Fix: "recover, or use a supervised worker", Severity: "WARNING",
			Pattern: must(`^\s*go func\(`), Semgrep: `go func() { ... }()`},
	}
}

var skipDirs = map[string]bool{
	".git": true, "vendor": true, "node_modules": true, "testdata": true,
}

// Probe measures every candidate across every repo.
func Probe(repos []string, cands []Candidate, t Thresholds) ([]Result, error) {
	results := make([]Result, len(cands))
	for i, c := range cands {
		results[i] = Result{Candidate: c, Rates: map[string]float64{}, Counts: map[string]int{}}
	}

	for _, repo := range repos {
		name := filepath.Base(strings.TrimSuffix(repo, "/"))
		counts := make([]int, len(cands))
		lines := 0

		err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
			// quality:false-positive returning nil from a WalkDir callback is the API's documented skip signal, not a swallowed error
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			// Tests are excluded throughout: test code has different norms and
			// including it would swamp the signal from production code.
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") ||
				strings.HasSuffix(p, ".pb.go") {
				return nil
			}
			src, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			text := string(src)
			fileLines := strings.Split(text, "\n")
			lines += len(fileLines)
			for i, c := range cands {
				for _, ln := range fileLines {
					if c.Pattern.MatchString(ln) && (c.Exclude == nil || !c.Exclude.MatchString(text)) {
						counts[i]++
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if lines == 0 {
			return nil, fmt.Errorf("%s: no Go source found", repo)
		}
		for i := range cands {
			results[i].Counts[name] = counts[i]
			results[i].Rates[name] = float64(counts[i]) / float64(lines) * 1000
		}
	}

	for i := range results {
		classify(&results[i], t)
	}
	sort.Slice(results, func(a, b int) bool {
		if results[a].Verdict != results[b].Verdict {
			return order(results[a].Verdict) < order(results[b].Verdict)
		}
		return results[a].Candidate.ID < results[b].Candidate.ID
	})
	return results, nil
}

func order(v Verdict) int {
	switch v {
	case Held:
		return 0
	case Divergent:
		return 1
	default:
		return 2
	}
}

func classify(r *Result, t Thresholds) {
	first := true
	for _, v := range r.Rates {
		if first {
			r.Max, r.Min, first = v, v, false
			continue
		}
		if v > r.Max {
			r.Max = v
		}
		if v < r.Min {
			r.Min = v
		}
	}
	switch {
	case r.Max < t.HeldBelow:
		r.Verdict = Held
	case r.Min == 0 || r.Max/r.Min > t.DivergentRatio:
		r.Verdict = Divergent
	default:
		r.Verdict = NotHeld
	}
}

// DraftRule emits a semgrep rule for a held candidate, with the evidence that
// justified it recorded in the metadata. A rule without its evidence is exactly
// what this whole exercise is meant to stop shipping.
func DraftRule(r Result) string {
	var b strings.Builder
	repos := make([]string, 0, len(r.Rates))
	for k := range r.Rates {
		repos = append(repos, k)
	}
	sort.Strings(repos)
	var ev []string
	for _, k := range repos {
		ev = append(ev, fmt.Sprintf("%.2f/kLOC", r.Rates[k]))
	}

	pat := r.Candidate.Semgrep
	if pat == "" {
		pat = "TODO: write the semgrep pattern"
	}
	fmt.Fprintf(&b, "rules:\n  - id: %s\n", r.Candidate.ID)
	fmt.Fprintf(&b, "    languages: [go]\n    severity: %s\n", r.Candidate.Severity)
	fmt.Fprintf(&b, "    message: >-\n      %s.\n      %s.\n", capitalise(r.Candidate.Why), capitalise(r.Candidate.Fix))
	fmt.Fprintf(&b, "    metadata:\n")
	fmt.Fprintf(&b, "      evidence: %s across %d codebases judged healthy\n",
		strings.Join(ev, ", "), len(repos))
	fmt.Fprintf(&b, "      confidence: medium\n")
	fmt.Fprintf(&b, "      note: >-\n        Derived by absence, then confirmed by reading real findings.\n")
	fmt.Fprintf(&b, "        Raise to high once precision data supports it.\n")
	fmt.Fprintf(&b, "    pattern: %s\n", pat)
	fmt.Fprintf(&b, "    paths:\n      exclude: [\"*_test.go\"]\n")
	return b.String()
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
