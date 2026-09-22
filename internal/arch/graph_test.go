package arch

import (
	"os/exec"
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

// The five analysis tools must stay free of third-party code. That property is
// why a team will run them in CI against their own source, and it is quietly
// lost the first time a convenience library gets imported into a shared
// package. judge is deliberately exempt: it calls a model, so it carries the
// SDK and the network call together.
//
// NOTICE and the README both state this; this test is what keeps them true.
func TestAnalysisToolsHaveNoThirdPartyCode(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to go list")
	}
	for _, cmd := range []string{"ratchet", "strata", "lens", "docket", "plumb"} {
		out, err := exec.Command("go", "list", "-deps", "../../cmd/"+cmd).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", cmd, err)
		}
		for _, pkg := range strings.Fields(string(out)) {
			root, _, _ := strings.Cut(pkg, "/")
			if !strings.Contains(root, ".") {
				continue // stdlib
			}
			if strings.HasPrefix(pkg, "github.com/sherzing/assay") {
				continue
			}
			t.Errorf("%s links third-party package %q — NOTICE and the README say it does not", cmd, pkg)
		}
	}
}
