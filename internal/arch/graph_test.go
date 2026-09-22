package arch

import (
	"strings"
	"testing"
)

// REGRESSION. The main module used to be inferred from the first package in the
// -deps stream carrying a Module.Path. But -deps emits DEPENDENCIES FIRST, so
// on any repository with third-party imports that is a library — every
// first-party package then falls outside "the module", the graph comes back
// nearly empty, and the check reports "0 violations".
//
// A silent pass is the one outcome this tool exists to prevent. On a real
// 62-package service it produced a 2-package graph and passed.
func TestMainModuleIsNotInferredFromTheFirstDependency(t *testing.T) {
	// A dependency, then the main module's packages — the real -deps order.
	stream := `{"ImportPath":"github.com/go-chi/chi/v5","Module":{"Path":"github.com/go-chi/chi/v5"},"GoFiles":["chi.go"]}
{"ImportPath":"example.com/shop/internal/impl","Module":{"Path":"example.com/shop"},"GoFiles":["db.go"],"Imports":["github.com/go-chi/chi/v5"]}
{"ImportPath":"example.com/shop/internal/domain","Module":{"Path":"example.com/shop"},"GoFiles":["d.go"],"Imports":["example.com/shop/internal/impl"]}
`
	g, err := parseGoList(strings.NewReader(stream), "example.com/shop", false)
	if err != nil {
		t.Fatal(err)
	}
	if g.Module != "example.com/shop" {
		t.Fatalf("module = %q, want the main module, not the first dependency", g.Module)
	}
	if len(g.Edges) != 2 {
		t.Fatalf("got %d packages, want the 2 first-party ones: %v", len(g.Edges), g.Packages())
	}
	for _, p := range g.Packages() {
		if strings.Contains(p, "go-chi") {
			t.Errorf("third-party package %q entered the graph; layers do not apply to libraries", p)
		}
	}
	// And the dependency must still be checkable end to end.
	d := mustParse(t, "layer domain internal/domain\nlayer infra internal/impl\nforbid domain -> infra\n")
	if vs := Check(d, g); len(vs) != 1 {
		t.Errorf("got %d violations, want 1 — the graph is present but the check found nothing", len(vs))
	}
}
