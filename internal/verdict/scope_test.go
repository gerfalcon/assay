package verdict

import (
	"go/parser"
	"go/token"
	"testing"

	"github.com/sherzing/assay/pkg/schema"
)

// FromComments is the entry point for the whole in-code judgement mechanism —
// it decides which findings an annotation excuses. Its scope is deliberately
// narrow, and the narrowness is the safety property: see
// TestAnnotationDoesNotReachIntoAFunctionBody.

func comments(t *testing.T, src string) map[int]V {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return FromComments(fset, f)
}

func TestAnnotationCoversItsOwnLineAndTheNext(t *testing.T) {
	// 1 package, 2 blank, 3 var, 4 annotation, 5 target
	got := comments(t, "package p\n\nvar a = 1\n// quality:accepted debt\nvar b = 2\nvar c = 3\n")

	for _, line := range []int{4, 5} {
		v, ok := got[line]
		if !ok {
			t.Errorf("line %d not covered; an annotation must excuse its own line and the one below", line)
			continue
		}
		if v.Verdict != schema.Accepted {
			t.Errorf("line %d: verdict = %q, want accepted", line, v.Verdict)
		}
		if v.Reason != "debt" {
			t.Errorf("line %d: reason = %q, want %q", line, v.Reason, "debt")
		}
	}
	if _, ok := got[6]; ok {
		t.Error("line 6 covered: the scope leaked two lines past the annotation")
	}
	if _, ok := got[3]; ok {
		t.Error("line 3 covered: an annotation must not reach backwards")
	}
}

// THE SAFETY PROPERTY. A judgement on a doc comment must not excuse everything
// inside the function. Otherwise one `// quality:false-positive` above a
// 200-line function silently suppresses every finding in it, and that is a
// blanket exemption hidden where no reviewer will read it as one.
//
// The two-line scope forces the annotation to sit next to the thing it
// excuses, which is what makes it reviewable in a diff.
func TestAnnotationDoesNotReachIntoAFunctionBody(t *testing.T) {
	src := "package p\n" + // 1
		"\n" + // 2
		"// quality:false-positive out of scope on purpose\n" + // 3
		"func f(x any) string {\n" + // 4  <- covered, it is line+1
		"\t_ = 1\n" + // 5
		"\treturn x.(string)\n" + // 6  <- must NOT be covered
		"}\n" // 7
	got := comments(t, src)

	if _, ok := got[4]; !ok {
		t.Error("the func signature line should be covered as line+1")
	}
	for _, line := range []int{5, 6, 7} {
		if _, ok := got[line]; ok {
			t.Errorf("line %d is covered by a doc-comment annotation — "+
				"a blanket exemption is hiding in a docstring", line)
		}
	}
}

func TestTrailingAnnotationCoversItsOwnLine(t *testing.T) {
	got := comments(t, "package p\n\nvar a = 1 // quality:wont-fix deliberate\n")
	v, ok := got[3]
	if !ok {
		t.Fatal("a trailing annotation did not cover its own line")
	}
	if v.Verdict != schema.WontFix {
		t.Errorf("verdict = %q, want wont-fix", v.Verdict)
	}
}

// An annotation must not overwrite the one before it just by being adjacent:
// the specific judgement on a line beats a neighbour's spillover.
func TestOwnLineBeatsTheNeighboursSpillover(t *testing.T) {
	src := "package p\n" + // 1
		"// quality:accepted first\n" + // 2 -> covers 2, spills to 3
		"// quality:false-positive second\n" + // 3 -> its own line wins
		"var a = 1\n" // 4 <- second spills here
	got := comments(t, src)

	if got[3].Verdict != schema.FalsePositive {
		t.Errorf("line 3 = %q (%q), want false-positive: an annotation's own line "+
			"must beat the previous line's spillover", got[3].Verdict, got[3].Reason)
	}
	if got[2].Verdict != schema.Accepted {
		t.Errorf("line 2 = %q, want accepted", got[2].Verdict)
	}
	if got[4].Verdict != schema.FalsePositive {
		t.Errorf("line 4 = %q, want false-positive spilled from line 3", got[4].Verdict)
	}
}

func TestUnannotatedFileYieldsNothing(t *testing.T) {
	got := comments(t, "package p\n\n// an ordinary comment\n// TODO: not a verdict\nvar a = 1\n")
	if len(got) != 0 {
		t.Errorf("got %v, want no judgements from ordinary comments", got)
	}
}

// Block comments are a legitimate place to put a judgement, and the line
// recorded must be where the comment STARTS.
func TestBlockCommentAnnotation(t *testing.T) {
	src := "package p\n\n/* quality:accepted paying this down in Q4 */\nvar a = 1\n"
	got := comments(t, src)
	if _, ok := got[3]; !ok {
		t.Errorf("block comment annotation not recognised: %v", got)
	}
	if _, ok := got[4]; !ok {
		t.Error("block comment annotation did not spill to the following line")
	}
}

// The Line field must report where the annotation was written, so a reviewer
// can be sent to it.
func TestVerdictCarriesItsSourceLine(t *testing.T) {
	got := comments(t, "package p\n\n\n\n// quality:accepted x\nvar a = 1\n")
	if got[5].Line != 5 {
		t.Errorf("Line = %d, want 5", got[5].Line)
	}
	// The spilled copy points back at the annotation, not at itself — the
	// reviewer needs the comment's location, not the finding's.
	if got[6].Line != 5 {
		t.Errorf("spilled Line = %d, want 5 (the annotation's own line)", got[6].Line)
	}
}
