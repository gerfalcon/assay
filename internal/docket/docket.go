// Package docket turns findings into tickets, and keeps them honest.
//
// It is a separate tool from ratchet and lens on purpose. ratchet gates CI, and
// a gate must be hermetic — a build should never fail because Linear is down.
// lens is read-only presentation, and making it write to an external system
// breaks that property. Ticketing has its own concerns: auth, idempotency,
// batching, lifecycle. That is the definition of a separate job.
//
// The idea that makes it work: the fingerprint that makes the ratchet function
// also makes ticket lifecycle function. Same key means create-once and
// close-when-fixed, with no duplicate on re-run.
package docket

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sherzing/assay/pkg/schema"
)

// GroupBy selects the cohort shape.
type GroupBy string

const (
	// ByTheme groups on the rule. This is the default because findings are
	// wildly repetitive: 90 naked type assertions is one ticket and one agent
	// PR, not 90 tickets.
	ByTheme GroupBy = "theme"
	// ByFile groups on the file — useful when several different rules fire in
	// one place and fixing them together is cheaper than separately.
	ByFile GroupBy = "file"
	// ByFinding is one ticket each. Almost always wrong; kept because someone
	// will have a workflow that needs it.
	ByFinding GroupBy = "finding"
)

// Group is a proposed ticket: a cohort of findings that should be fixed together.
type Group struct {
	Key          string
	Title        string
	Body         string
	Repo         string
	Fingerprints []string
	Findings     []schema.Finding
}

// Plan is what would be created, before anything leaves the machine.
type Plan struct {
	Groups        []Group
	SkippedFP     int      // findings a human marked false-positive
	SkippedTicket int      // findings already covered by an open ticket
	SkippedRules  []string // which rules the false positives came from
}

// Options controls planning.
type Options struct {
	GroupBy  GroupBy
	Max      int
	MinSize  int
	Severity schema.Severity // minimum severity to ticket
}

// Plan builds the cohorts.
//
// Two things are filtered out before anything is proposed:
//
//   - findings a human marked false-positive. Ticketing those puts noise in
//     front of a person, which is the exact failure this project exists to
//     prevent. If a rule produces them, fix the rule, do not file work.
//   - findings already covered by an existing ticket, so a re-run never
//     duplicates.
func BuildPlan(findings []schema.Finding, verdicts map[string]schema.Verdict,
	existing []schema.Ticket, opt Options) Plan {

	if opt.GroupBy == "" {
		opt.GroupBy = ByTheme
	}

	covered := map[string]bool{}
	for _, t := range existing {
		if t.State == "closed" {
			// Deliberate: a closed ticket still claims its fingerprints.
			// Someone closed it, which was a decision — do not re-file it.
			for _, fp := range t.Fingerprints {
				covered[fp] = true
			}
			continue
		}
		for _, fp := range t.Fingerprints {
			covered[fp] = true
		}
	}

	var plan Plan
	fpRules := map[string]bool{}
	byKey := map[string][]schema.Finding{}

	for _, f := range findings {
		if v, ok := verdicts[f.Fingerprint]; ok && v.Verdict == schema.FalsePositive {
			plan.SkippedFP++
			fpRules[f.Rule] = true
			continue
		}
		if covered[f.Fingerprint] {
			plan.SkippedTicket++
			continue
		}
		if !meetsSeverity(f.Severity, opt.Severity) {
			continue
		}
		byKey[groupKey(f, opt.GroupBy)] = append(byKey[groupKey(f, opt.GroupBy)], f)
	}
	for r := range fpRules {
		plan.SkippedRules = append(plan.SkippedRules, r)
	}
	sort.Strings(plan.SkippedRules)

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	// Biggest cohorts first: they clear the most findings per unit of review,
	// and the mechanical ones are usually the largest.
	sort.Slice(keys, func(i, j int) bool {
		if len(byKey[keys[i]]) != len(byKey[keys[j]]) {
			return len(byKey[keys[i]]) > len(byKey[keys[j]])
		}
		return keys[i] < keys[j]
	})

	for _, k := range keys {
		fs := byKey[k]
		if opt.MinSize > 0 && len(fs) < opt.MinSize {
			continue
		}
		plan.Groups = append(plan.Groups, makeGroup(k, fs, opt.GroupBy))
		if opt.Max > 0 && len(plan.Groups) >= opt.Max {
			break
		}
	}
	return plan
}

func groupKey(f schema.Finding, by GroupBy) string {
	switch by {
	case ByFile:
		return f.Repo + "\x00" + f.File
	case ByFinding:
		return f.Fingerprint
	default:
		return f.Repo + "\x00" + f.Rule
	}
}

func meetsSeverity(have, min schema.Severity) bool {
	if min == "" {
		return true
	}
	rank := map[schema.Severity]int{schema.SevInfo: 1, schema.SevWarn: 2, schema.SevError: 3}
	return rank[have] >= rank[min]
}

func makeGroup(key string, fs []schema.Finding, by GroupBy) Group {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].File != fs[j].File {
			return fs[i].File < fs[j].File
		}
		return fs[i].Line < fs[j].Line
	})

	g := Group{Key: key, Repo: fs[0].Repo, Findings: fs}
	for _, f := range fs {
		g.Fingerprints = append(g.Fingerprints, f.Fingerprint)
	}
	sort.Strings(g.Fingerprints)

	switch by {
	case ByFile:
		g.Title = fmt.Sprintf("Clean up %d issues in %s", len(fs), fs[0].File)
	case ByFinding:
		g.Title = fmt.Sprintf("%s: %s:%d", fs[0].Rule, fs[0].File, fs[0].Line)
	default:
		g.Title = fmt.Sprintf("Fix %d × %s", len(fs), fs[0].Rule)
	}
	if g.Repo != "" {
		g.Title = g.Repo + ": " + g.Title
	}
	g.Body = body(g, by)
	return g
}

// body writes something an engineer or an agent can act on directly: what, why,
// every location, and how to verify the fix.
func body(g Group, by GroupBy) string {
	var b strings.Builder
	f0 := g.Findings[0]

	fmt.Fprintf(&b, "%s\n\n", f0.Message)
	if f0.Suggest != "" {
		fmt.Fprintf(&b, "**Suggested fix:** %s\n\n", f0.Suggest)
	}
	fmt.Fprintf(&b, "**%d locations**", len(g.Findings))
	if by == ByTheme {
		fmt.Fprintf(&b, " of `%s`", f0.Rule)
	}
	b.WriteString("\n\n")

	const maxList = 60
	for i, f := range g.Findings {
		if i == maxList {
			fmt.Fprintf(&b, "- … and %d more\n", len(g.Findings)-maxList)
			break
		}
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		if f.Symbol != "" {
			fmt.Fprintf(&b, "- `%s` — %s\n", loc, f.Symbol)
		} else {
			fmt.Fprintf(&b, "- `%s`\n", loc)
		}
	}

	// The cohort is fixed, and saying so is what stops this becoming an
	// open-ended cleanup task nobody ever closes.
	b.WriteString("\n---\n\n")
	b.WriteString("This cohort is fixed at creation. `ratchet check` blocks new instances " +
		"from landing, so this ticket has a finish line.\n\n")
	b.WriteString("**Verify before opening a PR:**\n\n```sh\nratchet check .\n```\n\n")
	b.WriteString("A fix that makes something else worse fails that gate.\n")
	return b.String()
}

// Progress is how far a ticket has got.
type Progress struct {
	Ticket    schema.Ticket
	Total     int
	Resolved  int
	Remaining []string
	Done      bool
}

// Sync recomputes progress against a fresh scan.
//
// This is the answer to "if I ticket by theme, how do I know what is still
// open": the ticket carries its fingerprints, so progress is just set
// arithmetic against what still reproduces. Better than one-ticket-per-finding,
// because you get a progress bar rather than a binary.
func Sync(tickets []schema.Ticket, current []schema.Finding) []Progress {
	live := map[string]bool{}
	for _, f := range current {
		live[f.Fingerprint] = true
	}
	out := make([]Progress, 0, len(tickets))
	for _, t := range tickets {
		p := Progress{Ticket: t, Total: len(t.Fingerprints)}
		for _, fp := range t.Fingerprints {
			if live[fp] {
				p.Remaining = append(p.Remaining, fp)
			} else {
				p.Resolved++
			}
		}
		p.Done = p.Resolved == p.Total && p.Total > 0
		out = append(out, p)
	}
	return out
}

// ToTicket records a created ticket.
func ToTicket(g Group, provider, id, url string, by GroupBy) schema.Ticket {
	return schema.Ticket{
		Provider: provider, ID: id, URL: url, Repo: g.Repo,
		GroupBy: string(by), Group: g.Key, Title: g.Title,
		Fingerprints: g.Fingerprints, State: "open", TS: time.Now().UTC(),
	}
}

// Latest resolves the append-only ticket log to current state per ticket ID.
func Latest(all []schema.Ticket) []schema.Ticket {
	byID := map[string]schema.Ticket{}
	for _, t := range all {
		k := t.Provider + "\x00" + t.ID
		if cur, ok := byID[k]; !ok || t.TS.After(cur.TS) {
			byID[k] = t
		}
	}
	out := make([]schema.Ticket, 0, len(byID))
	for _, t := range byID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}
