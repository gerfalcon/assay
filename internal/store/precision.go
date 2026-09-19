package store

import (
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sherzing/assay/pkg/schema"
)

// RulePrecision is the evidence accumulated about one rule.
//
// Three buckets, because they mean different things and collapsing them loses
// the interesting signal:
//
//   - Fixed — the finding appeared, then stopped appearing. The strongest
//     confirmation available, because nobody fixes a false positive. It also
//     accrues PASSIVELY from normal work, which is what makes the dataset grow
//     without anyone running triage sessions.
//   - Carried — explicitly judged accepted or wont-fix. Real, but not being
//     fixed. A rule whose findings are overwhelmingly carried is not a bad rule;
//     it is pointing at something the ecosystem has decided to live with. That
//     is worth knowing and worth shipping as informational rather than as a gate.
//   - FalsePositive — the rule is wrong here. The only bucket that counts
//     against precision.
type RulePrecision struct {
	Rule          string  `json:"rule"`
	Fixed         int     `json:"fixed"`
	Carried       int     `json:"carried"`
	FalsePositive int     `json:"falsePositive"`
	Unjudged      int     `json:"unjudged"`
	Judged        int     `json:"judged"`
	Seen          int     `json:"seen"`
	Precision     float64 `json:"precision"`
	Coverage      float64 `json:"coverage"`
	Repos         int     `json:"repos"`
	// Orgs is the count of distinct organisations that contributed evidence.
	// The promotion bar requires >= 2, because a rule confirmed only inside one
	// company is that company's house style, not a shared defect.
	Orgs int `json:"orgs"`
	// Carriage is the share of confirmed findings nobody is fixing. High
	// carriage with high precision is the "known ecosystem problem" signature:
	// the rule is right, and the industry has given up on the underlying issue.
	Carriage float64 `json:"carriage"`
}

// Precision aggregates evidence per rule.
//
// A rule with no judged findings gets no precision at all rather than a
// flattering default. An unexamined rule has not earned a number.
func (s *Store) Precision() ([]RulePrecision, error) {
	verdicts, err := s.Verdicts()
	if err != nil {
		return nil, err
	}
	seen, latest, err := s.findingHistory()
	if err != nil {
		return nil, err
	}

	type acc struct {
		fixed, carried, fp, unjudged int
		repos, orgs                  map[string]bool
	}
	byRule := map[string]*acc{}
	get := func(rule string) *acc {
		a, ok := byRule[rule]
		if !ok {
			a = &acc{repos: map[string]bool{}, orgs: map[string]bool{}}
			byRule[rule] = a
		}
		return a
	}

	for fp, info := range seen {
		rule := info.rule
		if v, ok := verdicts[fp]; ok && v.Rule != "" {
			rule = v.Rule
		}
		if rule == "" {
			continue
		}
		a := get(rule)
		if info.repo != "" {
			a.repos[info.repo] = true
		}
		if v, ok := verdicts[fp]; ok && v.Org != "" {
			a.orgs[v.Org] = true
		}

		if v, ok := verdicts[fp]; ok {
			switch v.Verdict {
			case schema.FalsePositive:
				a.fp++
				continue
			case schema.Accepted, schema.WontFix:
				a.carried++
				continue
			}
		}
		// No explicit judgement. If it stopped appearing, someone fixed it,
		// which confirms the rule was right about it.
		if !latest[fp] {
			a.fixed++
			continue
		}
		a.unjudged++
	}

	out := make([]RulePrecision, 0, len(byRule))
	for rule, a := range byRule {
		confirmed := a.fixed + a.carried
		r := RulePrecision{
			Rule: rule, Fixed: a.fixed, Carried: a.carried,
			FalsePositive: a.fp, Unjudged: a.unjudged,
			Judged: confirmed + a.fp,
			Seen:   confirmed + a.fp + a.unjudged,
			Repos:  len(a.repos),
			Orgs:   len(a.orgs),
		}
		if r.Judged > 0 {
			r.Precision = float64(confirmed) / float64(r.Judged)
		}
		if r.Seen > 0 {
			r.Coverage = float64(r.Judged) / float64(r.Seen)
		}
		if confirmed > 0 {
			r.Carriage = float64(a.carried) / float64(confirmed)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Judged != out[j].Judged {
			return out[i].Judged > out[j].Judged
		}
		return out[i].Rule < out[j].Rule
	})
	return out, nil
}

type findingInfo struct {
	rule, repo  string
	first, last time.Time
}

// findingHistory walks stored findings and reports, per fingerprint, what rule
// it belonged to and whether it is still present in the most recent scan.
//
// "Still present" is judged against the latest scan per repo rather than a
// global latest, so a repo nobody has scanned lately does not read as having
// fixed everything.
func (s *Store) findingHistory() (map[string]findingInfo, map[string]bool, error) {
	seen := map[string]findingInfo{}
	lastScan := map[string]time.Time{} // repo -> most recent finding timestamp
	byRepoTS := map[string][]string{}  // repo|ts -> fingerprints

	err := s.walkDays("findings", time.Time{}, time.Time{}, func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		return schema.Decode(f, func(rec schema.Record) error {
			fd := rec.Finding
			if fd == nil {
				return nil
			}
			info, ok := seen[fd.Fingerprint]
			if !ok {
				info = findingInfo{rule: fd.Rule, repo: fd.Repo, first: fd.TS}
			}
			if fd.TS.After(info.last) {
				info.last = fd.TS
			}
			seen[fd.Fingerprint] = info

			if fd.TS.After(lastScan[fd.Repo]) {
				lastScan[fd.Repo] = fd.TS
			}
			key := fd.Repo + "\x00" + fd.TS.UTC().Format(time.RFC3339Nano)
			byRepoTS[key] = append(byRepoTS[key], fd.Fingerprint)
			return nil
		}, nil)
	})
	if err != nil {
		return nil, nil, err
	}

	// A fingerprint is "still live" if it appears in its repo's most recent scan.
	live := map[string]bool{}
	for key, fps := range byRepoTS {
		repo, tsStr, _ := strings.Cut(key, "\x00")
		ts, perr := time.Parse(time.RFC3339Nano, tsStr)
		if perr != nil || !ts.Equal(lastScan[repo]) {
			continue
		}
		for _, fp := range fps {
			live[fp] = true
		}
	}
	return seen, live, nil
}
