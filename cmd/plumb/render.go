package main

import (
	"fmt"
	"strings"

	"github.com/sherzing/assay/internal/arch"
)

// Rendering lives with the command, not in the analysis layer. The
// declaration says analysis produces findings and nothing else, and the judge
// held it to that: these two functions were its first findings against this
// repository. They could not move to the presenting layer either, because that
// layer may not reach analysis types. A command is not a layer, and rendering
// its own output is its job.

// formatDiff renders a classification for a CI comment. explained lists the
// dated Why entries this change added; a weakening with one is accepted.
func formatDiff(c arch.Change, detail []string, explained []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "architecture declaration: %s\n", c)
	for _, d := range detail {
		fmt.Fprintf(&b, "  %s\n", d)
	}
	if c.NeedsReview() {
		if len(explained) > 0 {
			fmt.Fprintf(&b, "\nThis weakens the declared architecture. It is explained under Why (%s),\n"+
				"which is what the gate asks for; the pull request review is the second reviewer.\n",
				strings.Join(explained, ", "))
		} else {
			b.WriteString("\nThis weakens the declared architecture. It needs a second reviewer\n" +
				"and a `## Why` entry in the same file, starting with a bold date\n" +
				"(**2026-01-31.**), saying what changed and what for.\n")
		}
	}
	return b.String()
}

// formatDraft renders the draft as a block a person pastes into the arch block
// and then edits. The counts are comments: they are the evidence for each
// term, and the reader's job is to strike the ones that are homonyms or noise.
func formatDraft(lines []arch.DraftLine) string {
	var b strings.Builder
	width := 0
	for _, l := range lines {
		if len(l.Layer) > width {
			width = len(l.Layer)
		}
	}
	for _, l := range lines {
		var ts, ev []string
		for _, t := range l.Terms {
			ts = append(ts, t.Term)
			ev = append(ev, fmt.Sprintf("%s:%d", t.Term, t.Count))
		}
		fmt.Fprintf(&b, "owns %-*s %s   # %d decls; %s\n", width, l.Layer, strings.Join(ts, " "), l.Decls, strings.Join(ev, " "))
		if arch.NotAContext(l.Layer) {
			b.WriteString("   # ↑ utility, not a context — strike this whole line\n")
		}
	}
	return b.String()
}
