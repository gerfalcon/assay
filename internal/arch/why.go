package arch

import (
	"regexp"
	"strings"
)

// A loosening needs a second reviewer and an entry under Why. The reviewer is
// the pull request review, which the forge records. The entry is checkable
// here: a dated paragraph under `## Why` that the previous version of the
// document did not have. A loosening that arrives with one is accepted; a
// loosening without one is not. What the gate enforces is that whoever weakened
// the architecture wrote down why, in the file the next reader will open.

var whyDateRe = regexp.MustCompile(`(?m)^\*\*(\d{4}-\d{2}-\d{2})\.?\*\*`)

// WhyEntries returns the dates of the entries under the `## Why` heading, in
// document order. An entry is a paragraph starting with a bold date:
// **2026-09-22.** Text before the heading is ignored, so a date in the prose
// above the block does not count as an explanation.
func WhyEntries(doc string) []string {
	i := strings.Index(doc, "\n## Why")
	if i < 0 {
		return nil
	}
	body := doc[i:]
	// Stop at the next second-level heading, if any.
	if j := strings.Index(body[1:], "\n## "); j >= 0 {
		body = body[:j+1]
	}
	var dates []string
	for _, m := range whyDateRe.FindAllStringSubmatch(body, -1) {
		dates = append(dates, m[1])
	}
	return dates
}

// NewWhyEntries returns the dated entries present in new and absent from old.
func NewWhyEntries(old, new string) []string {
	seen := map[string]bool{}
	for _, d := range WhyEntries(old) {
		seen[d] = true
	}
	var out []string
	for _, d := range WhyEntries(new) {
		if !seen[d] {
			out = append(out, d)
			seen[d] = true
		}
	}
	return out
}
