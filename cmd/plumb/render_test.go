package main

import (
	"strings"
	"testing"

	"github.com/sherzing/assay/internal/arch"
)

func TestFormatDiffWarnsOnlyWhenWeakened(t *testing.T) {
	out := formatDiff(arch.Loosening, []string{"- forbid s -> infra"}, nil)
	if !strings.Contains(out, "second reviewer") {
		t.Errorf("a loosening did not ask for review:\n%s", out)
	}
	if strings.Contains(formatDiff(arch.Tightening, []string{"+ forbid s -> infra"}, nil), "second reviewer") {
		t.Error("a tightening asked for review; the process will be routed around")
	}
	if out := formatDiff(arch.Loosening, nil, []string{"2026-09-22"}); !strings.Contains(out, "explained under Why (2026-09-22)") {
		t.Errorf("an explained loosening must say so:\n%s", out)
	}
}

// The draft must flag a utility layer inline, where the reader is deciding.
func TestFormatDraftMarksUtilityLayers(t *testing.T) {
	out := formatDraft([]arch.DraftLine{
		{Layer: "pkg/utils", Decls: 39, Terms: []arch.TermCount{{Term: "content", Count: 6}}},
		{Layer: "internal/cart", Decls: 120, Terms: []arch.TermCount{{Term: "cart", Count: 15}}},
	})
	if !strings.Contains(out, "utility, not a context") {
		t.Errorf("pkg/utils not marked:\n%s", out)
	}
	cartLine := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "internal/cart") {
			cartLine = l
		}
	}
	if strings.Contains(cartLine, "utility") {
		t.Errorf("a real domain was marked as utility: %q", cartLine)
	}
}
