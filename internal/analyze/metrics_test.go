package analyze

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// parseFn parses a single function and returns its declaration.
func parseFn(t *testing.T, src string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\n"+src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			return fd
		}
	}
	t.Fatal("no func found")
	return nil
}

// These assert exact numbers on purpose. A metric whose definition drifts
// silently is worse than no metric: the trend line stays smooth while its
// meaning changes underneath, and every historical datapoint becomes a lie.
func TestCyclomatic(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"empty", `func f() {}`, 1},
		{"single if", `func f(a int) { if a > 0 { } }`, 2},
		{"if else", `func f(a int) { if a > 0 { } else { } }`, 2}, // else adds no path
		{"two ifs", `func f(a int) { if a > 0 {}; if a < 0 {} }`, 3},
		{"for", `func f() { for i := 0; i < 10; i++ {} }`, 2},
		{"range", `func f(xs []int) { for range xs {} }`, 2},
		{"and", `func f(a, b bool) { if a && b {} }`, 3},
		{"or chain", `func f(a, b, c bool) { if a || b || c {} }`, 4},
		{"switch two cases", `func f(a int) { switch a { case 1: case 2: } }`, 3},
		{"switch with default", `func f(a int) { switch a { case 1: default: } }`, 2}, // default is free
		{"nested", `func f(a, b int) { if a > 0 { if b > 0 {} } }`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cyclomatic(parseFn(t, tc.src)); got != tc.want {
				t.Errorf("cyclomatic = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCognitive(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"empty", `func f() {}`, 0},
		{"flat if", `func f(a int) { if a > 0 {} }`, 1},
		// else costs a flat 1: the reader already paid for the structure.
		{"if else", `func f(a int) { if a > 0 {} else {} }`, 2},
		// Nested if costs 1 for itself plus 1 for sitting one level deep.
		{"nested if", `func f(a, b int) { if a > 0 { if b > 0 {} } }`, 3},
		{"triple nested", `func f(a, b, c int) { if a > 0 { if b > 0 { if c > 0 {} } } }`, 6},
		// One run of && is one idea, regardless of operand count.
		{"and run", `func f(a, b, c bool) { if a && b && c {} }`, 2},
		// Mixed operators are two ideas.
		{"mixed ops", `func f(a, b, c bool) { if a && b || c {} }`, 3},
		{"for", `func f() { for {} }`, 1},
		{"nested for in if", `func f(a int) { if a > 0 { for {} } }`, 3},
		// A flat switch is cheap here but expensive on cyclomatic — the whole
		// reason both metrics exist.
		{"flat switch", `func f(a int) { switch a { case 1: case 2: case 3: } }`, 1},
		{"bare break is free", `func f() { for { break } }`, 1},
		{"goto is not", `func f() { goto L; L: }`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cognitive(parseFn(t, tc.src)); got != tc.want {
				t.Errorf("cognitive = %d, want %d", got, tc.want)
			}
		})
	}
}

// Cyclomatic and cognitive must diverge on a flat switch, or we have
// accidentally implemented the same metric twice under two names.
func TestCognitiveDivergesFromCyclomatic(t *testing.T) {
	flat := parseFn(t, `func f(a int) { switch a { case 1: case 2: case 3: case 4: } }`)
	nested := parseFn(t, `func f(a, b, c int) { if a > 0 { if b > 0 { if c > 0 {} } } }`)

	fc, fg := cyclomatic(flat), cognitive(flat)
	nc, ng := cyclomatic(nested), cognitive(nested)

	if !(fc > fg) {
		t.Errorf("flat switch: expected cyclomatic (%d) > cognitive (%d)", fc, fg)
	}
	if !(ng > nc) {
		t.Errorf("nested ifs: expected cognitive (%d) > cyclomatic (%d)", ng, nc)
	}
}

func TestMaxNesting(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"none", `func f() { x := 1; _ = x }`, 0},
		{"one", `func f(a int) { if a > 0 {} }`, 1},
		{"two", `func f(a, b int) { if a > 0 { if b > 0 {} } }`, 2},
		{"three", `func f(a, b, c int) { for { if a > 0 { for b < c {} } } }`, 3},
		{"else does not deepen", `func f(a int) { if a > 0 {} else {} }`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxNesting(parseFn(t, tc.src)); got != tc.want {
				t.Errorf("maxNesting = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCountStatementsIgnoresFormatting(t *testing.T) {
	tight := parseFn(t, `func f() { a := 1; b := 2; _ = a; _ = b }`)
	loose := parseFn(t, `func f() {
	// a comment that should not count
	a := 1

	// another
	b := 2
	_ = a
	_ = b
}`)
	if got, want := countStatements(tight.Body), countStatements(loose.Body); got != want {
		t.Errorf("statement count changed with formatting: %d vs %d", got, want)
	}
}

func TestFieldCount(t *testing.T) {
	fd := parseFn(t, `func f(a, b int, c string) (int, error) { return 0, nil }`)
	if got := fieldCount(fd.Type.Params); got != 3 {
		t.Errorf("params = %d, want 3", got)
	}
	if got := fieldCount(fd.Type.Results); got != 2 {
		t.Errorf("results = %d, want 2", got)
	}
}
