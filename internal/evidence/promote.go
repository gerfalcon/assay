package evidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Thresholds a rule must clear to enter the core pack.
//
// Each one exists to defeat a specific way a bad rule gets adopted:
//
//	MinOrgs   — stops one team's house style becoming everyone's problem
//	MinJudged — stops a rule shipping on three anecdotes
//	MinRepos  — stops a rule tuned to one codebase
//	MinPrecision — stops a rule that is mostly wrong
type Thresholds struct {
	MinOrgs      int
	MinJudged    int
	MinRepos     int
	MinPrecision float64
	// HighCarriage is the share of confirmed findings being carried rather than
	// fixed, above which a rule is recommended as informational instead of as a
	// gate. It is not a failure — see Assess.
	HighCarriage float64
}

// DefaultThresholds are the published bar. Changing these changes what the
// corpus means, so they live in one place and are stated in CONTRIBUTING.md.
func DefaultThresholds() Thresholds {
	return Thresholds{
		MinOrgs: 2, MinJudged: 50, MinRepos: 3,
		MinPrecision: 0.80, HighCarriage: 0.80,
	}
}

// Outcome is the recommendation for a rule.
type Outcome string

const (
	// Promote — clears every bar.
	Promote Outcome = "promote"
	// PromoteInformational — the rule is right, but almost nobody fixes what it
	// finds. That is not a bad rule; it is a rule pointing at something the
	// ecosystem has decided to live with, like a framework defect everyone works
	// around. Shipping it as severity INFO is useful; shipping it as a gate
	// would just fail builds over something nobody intends to change.
	PromoteInformational Outcome = "promote-informational"
	// NotYet — plausible, but the evidence is not there yet.
	NotYet Outcome = "not-yet"
	// Reject — enough evidence to say it is mostly wrong. Rules must be able to
	// lose, or the corpus only ever grows and its average quality only ever
	// falls.
	Reject Outcome = "reject"
)

// Corpus is the aggregated evidence for one rule across every contributor.
type Corpus struct {
	Rule          string
	Orgs          []string
	Periods       []string
	Repos         int
	Fixed         int
	Carried       int
	FalsePositive int
	Judged        int
	Precision     float64
	Carriage      float64
}

// Assessment is the answer, as data rather than prose, so CI can act on it.
type Assessment struct {
	Corpus  Corpus
	Outcome Outcome
	Met     []string
	Unmet   []string
	Notes   []string
}

// Aggregate merges records into one corpus per rule.
//
// Counts sum; precision is RECOMPUTED from the summed counts rather than
// averaged. Averaging per-org precision would let a tiny contributor with a
// lucky 1.00 drag up a rule that is mostly wrong everywhere else.
func Aggregate(recs []Record) map[string]*Corpus {
	out := map[string]*Corpus{}
	orgs := map[string]map[string]bool{}
	periods := map[string]map[string]bool{}

	for _, r := range recs {
		c, ok := out[r.Rule]
		if !ok {
			c = &Corpus{Rule: r.Rule}
			out[r.Rule] = c
			orgs[r.Rule] = map[string]bool{}
			periods[r.Rule] = map[string]bool{}
		}
		c.Repos += r.Repos
		c.Fixed += r.Fixed
		c.Carried += r.Carried
		c.FalsePositive += r.FalsePositive
		c.Judged += r.Judged
		orgs[r.Rule][r.Org] = true
		periods[r.Rule][r.Period] = true
	}
	for rule, c := range out {
		for o := range orgs[rule] {
			c.Orgs = append(c.Orgs, o)
		}
		for p := range periods[rule] {
			c.Periods = append(c.Periods, p)
		}
		sort.Strings(c.Orgs)
		sort.Strings(c.Periods)
		if c.Judged > 0 {
			c.Precision = float64(c.Fixed+c.Carried) / float64(c.Judged)
		}
		if conf := c.Fixed + c.Carried; conf > 0 {
			c.Carriage = float64(c.Carried) / float64(conf)
		}
	}
	return out
}

// Assess applies the thresholds.
func Assess(c Corpus, t Thresholds) Assessment {
	a := Assessment{Corpus: c}

	check := func(ok bool, met, unmet string) {
		if ok {
			a.Met = append(a.Met, met)
		} else {
			a.Unmet = append(a.Unmet, unmet)
		}
	}
	check(len(c.Orgs) >= t.MinOrgs,
		fmt.Sprintf("%d organisations", len(c.Orgs)),
		fmt.Sprintf("needs %d more organisations (has %d, wants %d)", t.MinOrgs-len(c.Orgs), len(c.Orgs), t.MinOrgs))
	check(c.Judged >= t.MinJudged,
		fmt.Sprintf("%d judged findings", c.Judged),
		fmt.Sprintf("needs %d more judged findings (has %d, wants %d)", t.MinJudged-c.Judged, c.Judged, t.MinJudged))
	check(c.Repos >= t.MinRepos,
		fmt.Sprintf("%d repositories", c.Repos),
		fmt.Sprintf("needs %d more repositories (has %d, wants %d)", t.MinRepos-c.Repos, c.Repos, t.MinRepos))
	check(c.Precision >= t.MinPrecision,
		fmt.Sprintf("precision %.2f", c.Precision),
		fmt.Sprintf("precision %.2f is below %.2f", c.Precision, t.MinPrecision))

	precisionOK := c.Precision >= t.MinPrecision
	enoughToJudge := c.Judged >= t.MinJudged

	switch {
	// Enough evidence AND mostly wrong. This is the only outcome that says no
	// rather than not-yet, and it must exist: without it the corpus only grows.
	case enoughToJudge && !precisionOK:
		a.Outcome = Reject
		a.Notes = append(a.Notes, fmt.Sprintf(
			"%d judged findings is enough to conclude, and %.0f%% of them were false positives. "+
				"Fix the rule and start the evidence again, or withdraw it.",
			c.Judged, (1-c.Precision)*100))

	case len(a.Unmet) > 0:
		a.Outcome = NotYet

	// Clears every bar, but almost nobody fixes what it finds.
	case c.Carriage >= t.HighCarriage:
		a.Outcome = PromoteInformational
		a.Notes = append(a.Notes, fmt.Sprintf(
			"%.0f%% of confirmed findings are carried, not fixed. The rule is right and the "+
				"underlying problem is one people work around rather than solve — a framework "+
				"defect, say. Ship it as severity INFO: knowing is useful, failing builds over "+
				"it is not.", c.Carriage*100))

	default:
		a.Outcome = Promote
	}

	if len(c.Periods) == 1 && c.Judged >= t.MinJudged {
		a.Notes = append(a.Notes,
			"all evidence comes from a single period — a rule that holds up over time is a "+
				"stronger bet than one measured in a single quarter")
	}
	return a
}

// LoadDir reads every evidence file under a directory tree.
func LoadDir(dir string) ([]Record, []error) {
	var recs []Record
	var errs []error
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".jsonl") && !strings.HasSuffix(p, ".json") {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, rerr))
			return nil
		}
		// Verify on read. Evidence that would not be accepted must not be
		// counted either, or the bar is enforced only at submission time and
		// anything already merged is trusted forever.
		for line, probs := range VerifyStream(data) {
			if Fatal(probs) {
				errs = append(errs, fmt.Errorf("%s:%d: %s", p, line, probs[0]))
			}
		}
		for _, l := range strings.Split(string(data), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			var r Record
			if jerr := json.Unmarshal([]byte(l), &r); jerr == nil && r.Schema == SchemaID {
				recs = append(recs, r)
			}
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return recs, errs
}
