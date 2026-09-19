// Package evidence is the aggregate a closed-source organisation can share.
//
// This is what makes the community model work for anyone who cannot open their
// code. A rule earns promotion on evidence from multiple organisations, and
// gathering that evidence must not require gathering anyone's source.
//
// So the export carries counts and nothing else. No paths, no fingerprints, no
// repository names, no reasons, no author, no fine-grained timestamps. What
// leaves the network is arithmetic.
//
// The verifier is an ALLOWLIST, not a blocklist. An unknown field is rejected
// rather than passed through, because the failure mode that matters is a future
// version of the exporter quietly adding something sensitive and every existing
// verifier waving it past.
package evidence

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SchemaID versions the wire format. Receivers reject anything they do not know.
const SchemaID = "assay-evidence/1"

// Record is one organisation's evidence about one rule, for one period.
//
// Every field here is a count, a ratio, or a coarse label. If a field is ever
// added that is not one of those three things, it does not belong in this type.
type Record struct {
	Schema        string  `json:"schema"`
	Rule          string  `json:"rule"`
	Org           string  `json:"org"`
	Period        string  `json:"period"` // YYYY-Qn — deliberately coarse
	Repos         int     `json:"repos"`  // a count, never names
	Fixed         int     `json:"fixed"`
	Carried       int     `json:"carried"`
	FalsePositive int     `json:"falsePositive"`
	Judged        int     `json:"judged"`
	Precision     float64 `json:"precision"`
}

// allowed is the complete set of permitted JSON keys. Anything else is refused.
var allowed = map[string]bool{
	"schema": true, "rule": true, "org": true, "period": true,
	"repos": true, "fixed": true, "carried": true,
	"falsePositive": true, "judged": true, "precision": true,
}

// Patterns that indicate something escaped that should not have.
var (
	looksLikePath        = regexp.MustCompile(`[/\\]|\.(go|cs|dart|ts|js|py|java|kt|rb|php|rs)\b`)
	looksLikeFingerprint = regexp.MustCompile(`^[0-9a-f]{12,}$`)
	looksLikeTicket      = regexp.MustCompile(`\b[A-Z]{2,}-\d+\b`)
	looksLikeEmail       = regexp.MustCompile(`@[\w.-]+\.\w+`)
)

// Problem is one reason a record cannot be shared.
type Problem struct {
	Field string
	Msg   string
	Fatal bool
}

func (p Problem) String() string {
	sev := "warn"
	if p.Fatal {
		sev = "REJECT"
	}
	return fmt.Sprintf("%-6s %-14s %s", sev, p.Field, p.Msg)
}

// Verify checks one record's raw JSON.
//
// Takes bytes rather than a decoded Record on purpose: decoding into a struct
// silently discards unknown fields, which is exactly the leak this is meant to
// catch.
func Verify(raw []byte) []Problem {
	var probs []Problem
	add := func(field, msg string, fatal bool) {
		probs = append(probs, Problem{field, msg, fatal})
	}

	var loose map[string]any
	if err := json.Unmarshal(raw, &loose); err != nil {
		return []Problem{{"-", "not valid JSON: " + err.Error(), true}}
	}

	// Allowlist first. This is the load-bearing check.
	var unknown []string
	for k := range loose {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		add(k, "unknown field — only counts, ratios and coarse labels may be shared", true)
	}

	// Then check the values of the fields we do allow, because a permitted
	// field can still carry something it should not. A rule id is free text.
	//
	// "schema" is excluded: it is checked for exact equality against SchemaID
	// below, so scanning it is redundant — and it contains a slash, which the
	// path detector rightly objects to. Caught by the tests, which is the
	// argument for having written them adversarially.
	for _, k := range []string{"rule", "org", "period"} {
		v, ok := loose[k].(string)
		if !ok {
			continue
		}
		switch {
		case looksLikePath.MatchString(v):
			add(k, fmt.Sprintf("looks like a file path: %q", v), true)
		case looksLikeFingerprint.MatchString(v):
			add(k, fmt.Sprintf("looks like a fingerprint: %q", v), true)
		case looksLikeTicket.MatchString(v):
			add(k, fmt.Sprintf("contains what looks like a ticket reference: %q", v), true)
		case looksLikeEmail.MatchString(v):
			add(k, fmt.Sprintf("contains an email address: %q", v), true)
		}
	}

	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return append(probs, Problem{"-", "does not match the record shape: " + err.Error(), true})
	}

	if r.Schema != SchemaID {
		add("schema", fmt.Sprintf("is %q, want %q", r.Schema, SchemaID), true)
	}
	if r.Rule == "" {
		add("rule", "missing", true)
	}
	if r.Org == "" {
		add("org", "missing — evidence with no attribution cannot count toward the multi-org bar", true)
	}
	if !regexp.MustCompile(`^\d{4}-Q[1-4]$`).MatchString(r.Period) {
		add("period", fmt.Sprintf("is %q, want YYYY-Qn — finer granularity narrows who you are", r.Period), true)
	}
	for f, v := range map[string]int{
		"repos": r.Repos, "fixed": r.Fixed, "carried": r.Carried,
		"falsePositive": r.FalsePositive, "judged": r.Judged,
	} {
		if v < 0 {
			add(f, "negative count", true)
		}
	}
	if got := r.Fixed + r.Carried + r.FalsePositive; got != r.Judged {
		add("judged", fmt.Sprintf("is %d but fixed+carried+falsePositive is %d", r.Judged, got), true)
	}

	// Non-fatal: weak evidence is worth flagging, and very small counts are
	// also more identifying when combined with an org name.
	if r.Judged > 0 && r.Judged < 10 {
		add("judged", fmt.Sprintf("only %d judged findings — weak evidence, and small counts "+
			"are more identifying when paired with an org", r.Judged), false)
	}
	if r.Repos == 1 {
		add("repos", "a single repository — the rule may be tuned to one codebase", false)
	}
	return probs
}

// Fatal reports whether any problem blocks sharing.
func Fatal(probs []Problem) bool {
	for _, p := range probs {
		if p.Fatal {
			return true
		}
	}
	return false
}

// VerifyStream checks a JSONL file of records and returns problems per line.
func VerifyStream(data []byte) map[int][]Problem {
	out := map[int][]Problem{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if p := Verify([]byte(line)); len(p) > 0 {
			out[i+1] = p
		}
	}
	return out
}
