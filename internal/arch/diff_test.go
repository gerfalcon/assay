package arch

import (
	"strings"
	"testing"
)

const base = `
layer domain   internal/domain
layer infra    internal/impl
forbid domain -> infra
`

// THE ASYMMETRY IS THE POINT. A change process people route around is worse
// than none, so tightening must cost nothing — it is the common case and the
// one nobody needs protecting from. Only loosening buys a second reviewer.
func TestTighteningNeedsNoReviewAndLooseningDoes(t *testing.T) {
	cases := []struct {
		name string
		to   string
		want Change
	}{
		{"identical", base, NoChange},
		{"reordered", "layer infra internal/impl\nlayer domain internal/domain\nforbid domain -> infra\n", NoChange},
		{"added forbid", base + "layer svc internal/service\nforbid svc -> infra\n", Tightening},
		{"layer widened", "layer domain internal/domain internal/core\nlayer infra internal/impl\nforbid domain -> infra\n", Tightening},
		{"infra widened", "layer domain internal/domain\nlayer infra internal/impl internal/db\nforbid domain -> infra\n", Tightening},
		{"forbid removed", "layer domain internal/domain\nlayer infra internal/impl\nlayer x y\nforbid domain -> x\n", Mixed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, detail := Diff(mustParse(t, base), mustParse(t, c.to))
			if got != c.want {
				t.Fatalf("Diff = %v, want %v (detail: %v)", got, c.want, detail)
			}
			if got.NeedsReview() != (c.want == Loosening || c.want == Mixed) {
				t.Errorf("NeedsReview() = %v for %v", got.NeedsReview(), got)
			}
		})
	}
}

// Narrowing a layer releases packages from its rules, so it is a loosening even
// though the declaration got shorter. Length is not the signal.
func TestNarrowingALayerIsLoosening(t *testing.T) {
	from := mustParse(t, "layer domain internal/domain internal/core\nlayer infra internal/impl\nforbid domain -> infra\n")
	to := mustParse(t, base)
	got, detail := Diff(from, to)
	if got != Loosening {
		t.Errorf("Diff = %v, want loosening (detail: %v)", got, detail)
	}
	if !got.NeedsReview() {
		t.Error("narrowing a layer must need review: packages silently left the rule")
	}
}

func TestDroppingALayerEntirelyIsLoosening(t *testing.T) {
	from := mustParse(t, base+"layer svc internal/service\nforbid svc -> infra\n")
	got, _ := Diff(from, mustParse(t, base))
	if got != Loosening {
		t.Errorf("Diff = %v, want loosening", got)
	}
}

// Reordering must be NoChange, or every cosmetic edit demands a second
// reviewer and the process gets ignored.
func TestReorderingIsNotAChange(t *testing.T) {
	a := mustParse(t, "layer a x\nlayer b y\nlayer c z\nforbid a -> b\nforbid a -> c\n")
	b := mustParse(t, "layer c z\nlayer a x\nlayer b y\nforbid a -> c\nforbid a -> b\n")
	if got, detail := Diff(a, b); got != NoChange {
		t.Errorf("Diff = %v (%v), want no change", got, detail)
	}
	// Path order within a layer is equally cosmetic.
	c := mustParse(t, "layer a x y\nlayer b z\nforbid a -> b\n")
	dd := mustParse(t, "layer a y x\nlayer b z\nforbid a -> b\n")
	if got, _ := Diff(c, dd); got != NoChange {
		t.Errorf("path reordering read as %v, want no change", got)
	}
}

func TestFormatDiffWarnsOnlyWhenWeakened(t *testing.T) {
	c, detail := Diff(mustParse(t, base+"layer s q\nforbid s -> infra\n"), mustParse(t, base))
	out := FormatDiff(c, detail)
	if !strings.Contains(out, "second reviewer") {
		t.Errorf("a loosening did not ask for review:\n%s", out)
	}
	c2, d2 := Diff(mustParse(t, base), mustParse(t, base+"layer s q\nforbid s -> infra\n"))
	if strings.Contains(FormatDiff(c2, d2), "second reviewer") {
		t.Error("a tightening asked for review; the process will be routed around")
	}
}

func TestDiffDetailNamesWhatMoved(t *testing.T) {
	_, detail := Diff(mustParse(t, base), mustParse(t, base+"layer svc internal/service\nforbid svc -> infra\n"))
	joined := strings.Join(detail, "\n")
	if !strings.Contains(joined, "svc -> infra") {
		t.Errorf("detail does not name the added rule:\n%s", joined)
	}
}
