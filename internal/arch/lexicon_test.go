package arch

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestStems(t *testing.T) {
	cases := map[string][]string{
		"OrderRepository":     {"order"},
		"GetCartItemsAsync":   {"cart"},
		"SubmitRating":        {"submit", "rating"},
		"HTTPServer":          {"http", "server"},
		"payment_sources_dao": {"payment", "source", "dao"},
		"Status":              {"status"}, // not "statu"
		"Categories":          {"category"},
		"ID":                  nil,
		"Get":                 nil,
	}
	for in, want := range cases {
		got := Stems(in)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("Stems(%q) = %v, want %v", in, got, want)
		}
	}
}

const owned = `
layer cart    internal/cart
layer rating  internal/rating
layer web     internal/web
owns cart     cart checkout
owns rating   rating review
`

// THE LOAD-BEARING TEST for ownership. Layering cannot see this: cart imports
// nothing it should not. It has simply grown a rating concept.
func TestDriftIsCaughtAndConsumersAreNot(t *testing.T) {
	d := mustParse(t, owned)
	decls := []Declaration{
		{Layer: "cart", Name: "Cart", File: "internal/cart/cart.go", Line: 3},
		{Layer: "cart", Name: "SubmitRating", File: "internal/cart/rate.go", Line: 9},
		{Layer: "cart", Name: "ReviewSummary", File: "internal/cart/rate.go", Line: 20},
		{Layer: "rating", Name: "Rating", File: "internal/rating/rating.go", Line: 1},
		// web has no owns line: it may name anything.
		{Layer: "web", Name: "RatingPage", File: "internal/web/rating.go", Line: 1},
		{Layer: "web", Name: "CartPage", File: "internal/web/cart.go", Line: 1},
	}
	got := CheckOwns(d, decls)
	if len(got) != 2 {
		t.Fatalf("want 2 drifts, got %d: %+v", len(got), got)
	}
	if got[0].Name != "SubmitRating" || got[0].Owner != "rating" || got[0].Term != "rating" {
		t.Errorf("first drift = %+v", got[0])
	}
	if got[1].Name != "ReviewSummary" || got[1].Term != "review" {
		t.Errorf("second drift = %+v", got[1])
	}
	if got[0].Fingerprint() == got[1].Fingerprint() {
		t.Error("distinct declarations must have distinct fingerprints")
	}
	// Moving the file within the layer must not change the fingerprint.
	moved := got[0]
	moved.File, moved.Line = "internal/cart/other.go", 99
	if moved.Fingerprint() != got[0].Fingerprint() {
		t.Error("fingerprint must survive a move within the layer")
	}
}

func TestParseOwns(t *testing.T) {
	d := mustParse(t, owned)
	if strings.Join(d.Owns["cart"], " ") != "cart checkout" || strings.Join(d.Owns["rating"], " ") != "rating review" {
		t.Errorf("owns = %v", d.Owns)
	}
	if strings.Join(d.OwnsOrder, " ") != "cart rating" {
		t.Errorf("order = %v", d.OwnsOrder)
	}

	bad := map[string]string{
		"undeclared layer": "layer a x\nowns b foo\n",
		"one owner":        "layer a x\nlayer b y\nowns a foo\nowns b foo\n",
		"needs a term":     "layer a x\nowns a\n",
		"enforces nothing": "layer a x\nlayer b y\n",
	}
	for name, body := range bad {
		if _, err := Parse(body, 1); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// An ownership-only declaration is valid: not every codebase is a Go module.
	if _, err := Parse("layer a x\nowns a foo\n", 1); err != nil {
		t.Errorf("owns-only declaration: %v", err)
	}
}

func TestDiffOwns(t *testing.T) {
	old := mustParse(t, "layer a x\nlayer b y\nowns a foo\n")
	more := mustParse(t, "layer a x\nlayer b y\nowns a foo bar\n")
	less := mustParse(t, "layer a x\nlayer b y\nowns b foo\n")
	if c, _ := Diff(old, more); c != Tightening {
		t.Errorf("adding a term: %s, want tightening", c)
	}
	if c, _ := Diff(old, less); c != Mixed {
		t.Errorf("transferring a term: %s, want mixed", c)
	}
}

func TestScanDeclsAcrossLanguages(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	write("src/Cart/Cart.cs", "namespace Shop.Cart;\n\npublic class CartService {\n  public async Task<Rating> SubmitRatingAsync(int id) { }\n  private void helper() { }\n}\n")
	write("src/Cart/CartTests.cs", "public class CartTests { }\n")
	write("internal/rating/r.go", "package rating\n\ntype Rating struct{}\n\nfunc (r Rating) Score() int { return 0 }\n\nfunc New() Rating { return Rating{} }\n")
	write("internal/rating/r_test.go", "package rating\n\nfunc TestX() {}\n")
	write("vendor/x/x.go", "package x\n\ntype Vendored struct{}\n")
	d := mustParse(t, "layer cart src/Cart\nlayer rating internal/rating\nowns cart cart\nowns rating rating\n")

	got, err := ScanDecls(root, d, false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, dc := range got {
		names = append(names, dc.Layer+":"+dc.Name)
	}
	sort.Strings(names)
	want := "cart:CartService cart:SubmitRatingAsync rating:New rating:Rating rating:Score"
	if strings.Join(names, " ") != want {
		t.Errorf("decls = %v\nwant %s", names, want)
	}
	drifts := CheckOwns(d, got)
	if len(drifts) != 1 || drifts[0].Name != "SubmitRatingAsync" || drifts[0].File != "src/Cart/Cart.cs" || drifts[0].Line != 4 {
		t.Errorf("drift = %+v", drifts)
	}
}

func TestLearnDraftsFromDeclarations(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	write("internal/cart/a.go", "package cart\n\ntype Cart struct{}\ntype CartLine struct{}\nfunc NewCart() {}\nfunc CartTotal() {}\n")
	write("internal/rating/b.go", "package rating\n\ntype Rating struct{}\nfunc AddRating() {}\nfunc RatingAverage() {}\n")
	lines, err := Learn(root, nil, LearnOptions{Depth: 2, Min: 2, Share: 0.75})
	if err != nil {
		t.Fatal(err)
	}
	out := FormatDraft(lines)
	if !strings.Contains(out, "owns internal/cart") || !strings.Contains(out, " cart ") {
		t.Errorf("draft missing cart:\n%s", out)
	}
	if !strings.Contains(out, "owns internal/rating") || !strings.Contains(out, "rating") {
		t.Errorf("draft missing rating:\n%s", out)
	}
	// "line" appears once and "total"/"average" are single: below --min.
	if strings.Contains(out, "average") {
		t.Errorf("term below min leaked into draft:\n%s", out)
	}
}

// THE LOAD-BEARING TEST for extraction.
//
// Every language must admit the same THING — the public surface — however that
// language spells it. If one admits private declarations and another does not,
// the same misplacement counts in one codebase and not the other, and measured
// precision becomes language-dependent. That is fatal for a corpus whose whole
// currency is precision compared across organisations.
//
// Python's pattern once admitted `_private_helper` while Go's rejected
// `privateHelper`.
func TestPublicSurfaceOnlyAcrossLanguages(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Each file declares one PUBLIC and one PRIVATE thing carrying the same
	// concept. Only the public one may be extracted.
	write("cart/a.go", "package cart\n\ntype RatingBadge struct{}\n\nfunc ratingHelper() {}\n")
	write("cart/b.py", "class RatingBadge:\n    pass\n\ndef _rating_helper():\n    pass\n")
	write("cart/c.dart", "class RatingBadge {}\nclass _RatingHelper {}\n")
	write("cart/d.cs", "public class RatingBadge { }\nprivate class RatingHelperPrivate { }\n")
	write("cart/e.kt", "class RatingBadge\nprivate fun ratingHelper() {}\n")
	write("rating/r.go", "package rating\n\ntype Rating struct{}\n")

	d := mustParse(t, "layer cart cart\nlayer rating rating\nowns rating rating\n")
	got, err := ScanDecls(root, d, false)
	if err != nil {
		t.Fatal(err)
	}

	perFile := map[string][]string{}
	for _, dc := range got {
		perFile[filepath.Ext(dc.File)] = append(perFile[filepath.Ext(dc.File)], dc.Name)
	}
	for ext, names := range perFile {
		sort.Strings(names)
		for _, n := range names {
			// The privacy marker of every language represented here: a leading
			// underscore, a lower-case initial, or the word Private.
			if strings.HasPrefix(n, "_") || strings.Contains(n, "Private") ||
				(n != "" && strings.ToLower(n[:1]) == n[:1]) {
				t.Errorf("%s extracted the private declaration %q — the net must be the public surface in every language", ext, n)
			}
		}
	}

	// And the public one must be found in each, or the assertion above passes
	// vacuously on a language whose pattern matches nothing at all.
	for _, ext := range []string{".go", ".py", ".dart", ".cs", ".kt"} {
		found := false
		for _, n := range perFile[ext] {
			if n == "RatingBadge" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s did not extract the public RatingBadge: %v", ext, perFile[ext])
		}
	}
}

// A utility package holds mechanism, not domain, so its "vocabulary" is the
// generic words mechanism is written in. Letting one own `content format` would
// flag every ContentType in the codebase — so the draft must say strike the
// LINE, not prune its terms. Depth-based discovery nominates these freely: a
// real run drafted utils, client and web_utils alongside genuine domains.
func TestUtilityPackagesAreMarkedNotAContext(t *testing.T) {
	for _, layer := range []string{
		"pkg/utils", "pkg/util", "pkg/client", "pkg/web_utils", "internal/common",
		"src/Shared", "lib", "app/core", "pkg/helpers", "infra",
	} {
		if !NotAContext(layer) {
			t.Errorf("NotAContext(%q) = false, want true — a utility package owns no vocabulary", layer)
		}
	}
	// Real domains must not be swept up, or the marker becomes noise itself.
	for _, layer := range []string{
		"internal/cart", "internal/rating", "pkg/kafka", "internal/order-api",
		"src/Billing", "internal/compliance-worker", "pkg/monitoring",
	} {
		if NotAContext(layer) {
			t.Errorf("NotAContext(%q) = true, want false — that is a real domain", layer)
		}
	}
}

// The draft must flag a utility layer inline, where the reader is deciding.
func TestFormatDraftMarksUtilityLayers(t *testing.T) {
	out := FormatDraft([]DraftLine{
		{Layer: "pkg/utils", Decls: 39, Terms: []TermCount{{Term: "content", Count: 6}}},
		{Layer: "internal/cart", Decls: 120, Terms: []TermCount{{Term: "cart", Count: 15}}},
	})
	if !strings.Contains(out, "utility, not a context") {
		t.Errorf("pkg/utils not marked:\n%s", out)
	}
	// The marker must attach to the utility line only.
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

func TestWhyEntries(t *testing.T) {
	old := "# A\n\n**2026-01-01.** in the prose, does not count\n\n```arch\nlayer a a\nowns a x\n```\n\n## Why\n\n**2026-09-20.** first\n\n## Other\n\n**2026-09-21.** not under Why\n"
	new := strings.Replace(old, "**2026-09-20.** first\n", "**2026-09-20.** first\n\n**2026-09-22.** second\n", 1)
	if got := strings.Join(WhyEntries(old), ","); got != "2026-09-20" {
		t.Errorf("WhyEntries(old) = %q", got)
	}
	if got := strings.Join(NewWhyEntries(old, new), ","); got != "2026-09-22" {
		t.Errorf("NewWhyEntries = %q", got)
	}
	if !Accepted(Loosening, []string{"2026-09-22"}) || Accepted(Loosening, nil) || !Accepted(Tightening, nil) {
		t.Error("Accepted: a loosening passes only when explained")
	}
}
