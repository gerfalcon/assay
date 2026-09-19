package evidence

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/sherzing/assay/internal/store"
)

// Quarter formats a time as the coarse period label the schema requires.
//
// Coarse on purpose. A precise timestamp, combined with an org name and a rule,
// narrows who you are far more than the counts do.
func Quarter(t time.Time) string {
	u := t.UTC()
	return fmt.Sprintf("%d-Q%d", u.Year(), (int(u.Month())-1)/3+1)
}

// Options controls what is exported.
type Options struct {
	Org       string
	Period    string
	MinJudged int // below this, evidence is too weak and too identifying
	Rules     map[string]bool
}

// Build turns local precision data into shareable records.
//
// Everything that could identify a codebase is dropped here rather than
// filtered later: this function is the boundary, and the Record type has no
// field capable of carrying a path, a fingerprint or a repository name.
func Build(rows []store.RulePrecision, opt Options) ([]Record, []string, error) {
	if opt.Org == "" {
		return nil, nil, fmt.Errorf("an org is required: evidence with no attribution " +
			"cannot count toward the multi-organisation bar")
	}
	if opt.Period == "" {
		opt.Period = Quarter(time.Now())
	}
	if opt.MinJudged <= 0 {
		opt.MinJudged = 10
	}

	var out []Record
	var skipped []string
	for _, r := range rows {
		if opt.Rules != nil && !opt.Rules[r.Rule] {
			continue
		}
		if r.Judged < opt.MinJudged {
			skipped = append(skipped, fmt.Sprintf("%s (%d judged, below %d)", r.Rule, r.Judged, opt.MinJudged))
			continue
		}
		out = append(out, Record{
			Schema: SchemaID,
			Rule:   r.Rule,
			Org:    opt.Org,
			Period: opt.Period,
			Repos:  r.Repos,
			Fixed:  r.Fixed, Carried: r.Carried, FalsePositive: r.FalsePositive,
			Judged:    r.Judged,
			Precision: math.Round(r.Precision*100) / 100,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule < out[j].Rule })
	sort.Strings(skipped)
	return out, skipped, nil
}
