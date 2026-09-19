package analyze

import (
	"go/parser"
	"go/token"
	"testing"

	"github.com/sherzing/assay/internal/model"
)

func findingsFor(t *testing.T, src string) []model.Finding {
	t.Helper()
	fset := token.NewFileSet()
	full := "package p\n" + src
	f, err := parser.ParseFile(fset, "x.go", full, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := &smellPass{fset: fset, file: f, relPath: "x.go", src: []byte(full)}
	return p.run()
}

func countRule(fs []model.Finding, rule string) int {
	n := 0
	for _, f := range fs {
		if f.Rule == rule {
			n++
		}
	}
	return n
}

func TestNakedTypeAssertion(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"naked flagged", `func f(x any) { s := x.(string); _ = s }`, 1},
		{"comma-ok is safe", `func f(x any) { s, ok := x.(string); _ = s; _ = ok }`, 0},
		{"type switch is safe", `func f(x any) { switch x.(type) { case string: } }`, 0},
		{"typed switch binding is safe", `func f(x any) { switch v := x.(type) { case string: _ = v } }`, 0},
		{"naked in call arg", `func g(string) {}
func f(x any) { g(x.(string)) }`, 1},
		{"two naked", `func f(x, y any) { a := x.(int); b := y.(int); _, _ = a, b }`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := countRule(findingsFor(t, tc.src), RuleNakedAssert)
			if got != tc.want {
				t.Errorf("%s = %d findings, want %d", RuleNakedAssert, got, tc.want)
			}
		})
	}
}

func TestErrorSwallowed(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"blank assign", `func f() { var err error; _ = err }`, 1},
		{"empty branch", `func f() { var err error; if err != nil { } }`, 1},
		{"returns nil on error", `func f() error { var err error; if err != nil { return nil }; return nil }`, 1},
		{"proper handling", `func f() error { var err error; if err != nil { return err }; return nil }`, 0},
		{"wrapped is fine", `func f() error {
	var err error
	if err != nil { return fmt.Errorf("ctx: %w", err) }
	return nil
}`, 0},
		{"non-error blank is fine", `func f() { var x int; _ = x }`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := countRule(findingsFor(t, tc.src), RuleErrSwallowed)
			if got != tc.want {
				t.Errorf("%s = %d findings, want %d", RuleErrSwallowed, got, tc.want)
			}
		})
	}
}

func TestAnyInExportedSignature(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"exported any param", `func Do(x any) {}`, 1},
		{"exported interface{} param", `func Do(x interface{}) {}`, 1},
		{"exported any result", `func Do() any { return nil }`, 1},
		{"unexported is fine", `func do(x any) {}`, 0},
		{"concrete is fine", `func Do(x string) {}`, 0},
		{"non-empty interface is fine", `func Do(x interface{ Read() }) {}`, 0},
		// Variadic ...any is the fmt/sql/logging pass-through idiom. Real code
		// (a pgx wrapper in a real service) proved flagging it is pure noise.
		{"variadic any is idiomatic", `func Do(xs ...any) {}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := countRule(findingsFor(t, tc.src), RuleAnyExported)
			if got != tc.want {
				t.Errorf("%s = %d findings, want %d", RuleAnyExported, got, tc.want)
			}
		})
	}
}

func TestElseAfterReturn(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"else after return", `func f(a int) int { if a > 0 { return 1 } else { return 2 } }`, 1},
		{"no else", `func f(a int) int { if a > 0 { return 1 }; return 2 }`, 0},
		{"else without terminator", `func f(a int) { if a > 0 { _ = a } else { _ = a } }`, 0},
		{"else after continue", `func f(a int) { for { if a > 0 { continue } else { _ = a } } }`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := countRule(findingsFor(t, tc.src), RuleElseAfterJump)
			if got != tc.want {
				t.Errorf("%s = %d findings, want %d", RuleElseAfterJump, got, tc.want)
			}
		})
	}
}

// Every finding must carry something the reader can act on. A finding with no
// suggestion is a number wearing a costume.
func TestFindingsAreActionable(t *testing.T) {
	fs := findingsFor(t, `func Do(x any) { s := x.(string); _ = s }`)
	if len(fs) == 0 {
		t.Fatal("expected findings")
	}
	for _, f := range fs {
		if f.Suggest == "" {
			t.Errorf("rule %s produced no suggestion", f.Rule)
		}
		if f.Line == 0 {
			t.Errorf("rule %s produced no line number", f.Rule)
		}
		if f.Fingerprint == "" {
			t.Errorf("rule %s produced no fingerprint", f.Rule)
		}
	}
}

// The fingerprint must survive reformatting and line movement, or the first
// gofmt run after adoption looks like a wave of regressions and the gate dies.
func TestFingerprintStableAcrossLineMovement(t *testing.T) {
	a := findingsFor(t, `func f(x any) { s := x.(string); _ = s }`)
	b := findingsFor(t, `

// a comment added above, pushing everything down

func f(x any) {
	s := x.(string)
	_ = s
}`)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected 1 finding each, got %d and %d", len(a), len(b))
	}
	if a[0].Line == b[0].Line {
		t.Fatal("test is not exercising line movement")
	}
	if a[0].Fingerprint != b[0].Fingerprint {
		t.Errorf("fingerprint changed when only line position moved:\n  %s\n  %s",
			a[0].Fingerprint, b[0].Fingerprint)
	}
}

func TestRuleRegistryCoversEmittedRules(t *testing.T) {
	fs := findingsFor(t, `func Do(x any) int {
	var err error
	if err != nil { return 0 }
	s := x.(string)
	_ = s
	if x != nil { return 1 } else { return 2 }
}`)
	for _, f := range fs {
		if _, ok := Rules[f.Rule]; !ok {
			t.Errorf("rule %q emitted but missing from the Rules registry", f.Rule)
		}
	}
}

// The must* convention announces an intentional panic. regexp.MustCompile and
// template.Must are stdlib; flagging the idiom would flag Go itself.
func TestPanicExemptInMustFunctions(t *testing.T) {
	if got := countRule(findingsFor(t, `func mustMarshal(v any) []byte { panic("x") }`), RulePanicInLib); got != 0 {
		t.Errorf("must* function flagged for panic: got %d, want 0", got)
	}
	if got := countRule(findingsFor(t, `func MustParse(s string) int { panic("x") }`), RulePanicInLib); got != 0 {
		t.Errorf("Must* function flagged for panic: got %d, want 0", got)
	}
	if got := countRule(findingsFor(t, `func parse(s string) int { panic("x") }`), RulePanicInLib); got != 1 {
		t.Errorf("ordinary function not flagged for panic: got %d, want 1", got)
	}
}

// A codebase that already runs golangci-lint has an established way to say
// "I know". A tool that cannot be told no gets switched off.
func TestNolintSuppresses(t *testing.T) {
	src := `func f(x any) {
	s := x.(string) //nolint:forcetypeassert // checked upstream
	_ = s
}`
	if got := countRule(findingsFor(t, src), RuleNakedAssert); got != 0 {
		t.Errorf("nolint on the same line did not suppress: got %d, want 0", got)
	}
	if got := countRule(findingsFor(t, `func f(x any) {
	s := x.(string)
	_ = s
}`), RuleNakedAssert); got != 1 {
		t.Errorf("control case without nolint should fire: got %d, want 1", got)
	}
}
