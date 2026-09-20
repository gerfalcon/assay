package arch

import (
	"strings"
	"testing"
)

// graphOf builds a graph from a compact "pkg: dep dep" description, so a test
// states the shape it cares about instead of constructing a module on disk.
func graphOf(t *testing.T, module string, spec ...string) *Graph {
	t.Helper()
	g := &Graph{Module: module, Edges: map[string][]string{}, Files: map[string]string{}}
	for _, line := range spec {
		pkg, deps, _ := strings.Cut(line, ":")
		pkg = strings.TrimSpace(pkg)
		g.Edges[pkg] = strings.Fields(deps)
		g.Files[pkg] = pkg + "/file.go"
	}
	return g
}

const layered = `
layer domain   internal/domain
layer service  internal/service
layer infra    internal/impl
forbid domain -> infra
`

// THE LOAD-BEARING TEST.
//
// A direct-import check catches the first case and misses the second. They are
// the same architectural dependency — domain reaching infrastructure — and the
// second is what an ordinary "extract a helper" refactor produces. Nobody has
// to be working around anything for it to appear, which is exactly why a
// direct-only rule gives false confidence.
func TestIndirectDependencyIsCaught(t *testing.T) {
	d := mustParse(t, layered)

	direct := Check(d, graphOf(t, "example.com/shop",
		"internal/domain: internal/impl",
		"internal/impl:",
	))
	if len(direct) != 1 {
		t.Fatalf("direct import: got %d violations, want 1", len(direct))
	}

	indirect := Check(d, graphOf(t, "example.com/shop",
		"internal/domain: internal/helper",
		"internal/helper: internal/impl",
		"internal/impl:",
	))
	if len(indirect) != 1 {
		t.Fatalf("ONE HOP DEFEATED THE CHECK: got %d violations, want 1", len(indirect))
	}
	want := "internal/domain → internal/helper → internal/impl"
	if got := indirect[0].Message(); !strings.Contains(got, want) {
		t.Errorf("message does not show the chain.\n got: %s\nwant it to contain: %s", got, want)
	}
}

// The chain is the actionable part: it names the hop to cut.
func TestShortestChainIsReported(t *testing.T) {
	d := mustParse(t, layered)
	// Two routes from domain to impl: one hop via b, three via x/y/z.
	vs := Check(d, graphOf(t, "example.com/shop",
		"internal/domain: internal/b internal/x",
		"internal/b: internal/impl",
		"internal/x: internal/y",
		"internal/y: internal/z",
		"internal/z: internal/impl",
		"internal/impl:",
	))
	if len(vs) != 1 {
		t.Fatalf("got %d violations, want 1", len(vs))
	}
	if len(vs[0].Chain) != 3 {
		t.Errorf("chain = %v, want the shortest route (3 nodes)", vs[0].Chain)
	}
}

func TestCleanCodebaseHasNoViolations(t *testing.T) {
	d := mustParse(t, layered)
	vs := Check(d, graphOf(t, "example.com/shop",
		"internal/domain:",
		"internal/service: internal/domain",
		"internal/impl: internal/domain internal/service", // inward is allowed
		"cmd/server: internal/impl internal/service",      // unlayered, unconstrained
	))
	if len(vs) != 0 {
		t.Errorf("clean codebase reported %d violations: %+v", len(vs), vs)
	}
}

// Dependencies pointing inward are the whole point of a layered design, so the
// rule must be directional. A symmetric check would forbid the architecture it
// is supposed to enforce.
func TestRuleIsDirectional(t *testing.T) {
	d := mustParse(t, layered)
	vs := Check(d, graphOf(t, "example.com/shop",
		"internal/impl: internal/domain",
		"internal/domain:",
	))
	if len(vs) != 0 {
		t.Errorf("infra -> domain was flagged, but only domain -> infra is forbidden: %+v", vs)
	}
}

// One package reaching a layer by four routes is one architectural problem, not
// four. Reporting each route would make the fix look bigger than it is.
func TestOneViolationPerPackagePair(t *testing.T) {
	d := mustParse(t, layered)
	vs := Check(d, graphOf(t, "example.com/shop",
		"internal/domain: internal/a internal/b internal/c",
		"internal/a: internal/impl",
		"internal/b: internal/impl",
		"internal/c: internal/impl",
		"internal/impl:",
	))
	if len(vs) != 1 {
		t.Errorf("got %d violations for one package reaching one package, want 1", len(vs))
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	d := mustParse(t, layered)
	g := graphOf(t, "example.com/shop",
		"internal/domain/a: internal/impl",
		"internal/domain/b: internal/impl",
		"internal/domain/c: internal/impl",
		"internal/impl:",
	)
	var first []string
	for i := 0; i < 20; i++ {
		var got []string
		for _, v := range Check(d, g) {
			got = append(got, v.Message())
		}
		if i == 0 {
			first = got
			continue
		}
		if strings.Join(got, "|") != strings.Join(first, "|") {
			t.Fatalf("run %d differs — map iteration order leaked into the output", i)
		}
	}
}

// Fingerprints must survive a refactor that changes the route but not the fact.
// If they moved, a tolerated violation would reappear as new and the ratchet
// would cry wolf on a no-op change.
func TestFingerprintIgnoresTheChain(t *testing.T) {
	d := mustParse(t, layered)
	short := Check(d, graphOf(t, "example.com/shop",
		"internal/domain: internal/impl",
		"internal/impl:",
	))
	long := Check(d, graphOf(t, "example.com/shop",
		"internal/domain: internal/newhelper",
		"internal/newhelper: internal/another",
		"internal/another: internal/impl",
		"internal/impl:",
	))
	if short[0].Fingerprint() != long[0].Fingerprint() {
		t.Errorf("fingerprint changed when only the route changed:\n %s (%v)\n %s (%v)",
			short[0].Fingerprint(), short[0].Chain, long[0].Fingerprint(), long[0].Chain)
	}
}

func TestFingerprintChangesWithTheLayers(t *testing.T) {
	d := mustParse(t, layered)
	a := Check(d, graphOf(t, "example.com/shop", "internal/domain/x: internal/impl", "internal/impl:"))
	b := Check(d, graphOf(t, "example.com/shop", "internal/domain/y: internal/impl", "internal/impl:"))
	if a[0].Fingerprint() == b[0].Fingerprint() {
		t.Error("two different source packages share a fingerprint — the ratchet would conflate them")
	}
}

// A rule guarding a directory that no longer exists protects nobody, and
// everyone believes it does. Worth a warning: the directory may be about to be
// created.
func TestDeadLayersAreReported(t *testing.T) {
	d := mustParse(t, `
layer domain   internal/domain
layer legacy   internal/legacy
layer infra    internal/impl
forbid domain -> infra
forbid legacy -> infra
`)
	g := graphOf(t, "example.com/shop", "internal/domain:", "internal/impl:")
	dead := DeadLayers(d, g)
	if len(dead) != 1 || !strings.Contains(dead[0], "legacy") {
		t.Errorf("DeadLayers = %v, want the legacy layer named", dead)
	}
}

// A typo in ONE path of a multi-path layer leaves the layer matching packages,
// so a layer-level check stays silent while part of the rule guards nothing.
// This project's own declaration shipped with exactly that until plumb was run
// against it.
func TestDeadPathInsideALiveLayerIsReported(t *testing.T) {
	d := mustParse(t, `
layer presenting internal/report internal/typo
layer analysis   internal/analyze
forbid analysis -> presenting
`)
	g := graphOf(t, "example.com/shop", "internal/report:", "internal/analyze:")
	dead := DeadLayers(d, g)
	if len(dead) != 1 || !strings.Contains(dead[0], "internal/typo") {
		t.Errorf("DeadLayers = %v, want the dead PATH named, not just the layer", dead)
	}
}

// A package under a layer's path but with no edges must not be skipped: absence
// of imports is a clean result, not an unexamined one.
func TestPackageWithNoImports(t *testing.T) {
	d := mustParse(t, layered)
	vs := Check(d, graphOf(t, "example.com/shop", "internal/domain:", "internal/impl:"))
	if len(vs) != 0 {
		t.Errorf("got %+v", vs)
	}
}

// A cycle in the graph must not hang the traversal.
func TestCycleDoesNotHang(t *testing.T) {
	d := mustParse(t, layered)
	vs := Check(d, graphOf(t, "example.com/shop",
		"internal/domain: internal/a",
		"internal/a: internal/b",
		"internal/b: internal/a", // cycle, no route to impl
		"internal/impl:",
	))
	if len(vs) != 0 {
		t.Errorf("got %+v, want none — the cycle reaches nothing forbidden", vs)
	}
}

func TestSuggestNamesTheHopToCut(t *testing.T) {
	d := mustParse(t, layered)
	indirect := Check(d, graphOf(t, "example.com/shop",
		"internal/domain: internal/helper",
		"internal/helper: internal/impl",
		"internal/impl:",
	))
	if s := indirect[0].Suggest(); !strings.Contains(s, "internal/helper") {
		t.Errorf("suggestion for an indirect violation should name the intermediate package: %q", s)
	}
	direct := Check(d, graphOf(t, "example.com/shop",
		"internal/domain: internal/impl", "internal/impl:",
	))
	if s := direct[0].Suggest(); strings.Contains(s, "indirection") {
		t.Errorf("direct violation got the indirect advice: %q", s)
	}
}

func TestReportCarriesTheDeclarationSHA(t *testing.T) {
	d := mustParse(t, layered)
	d.SHA = "abc123"
	g := graphOf(t, "example.com/shop", "internal/domain: internal/impl", "internal/impl:")
	rep := Report(d, g, Check(d, g))
	if len(rep.Findings) != 1 {
		t.Fatalf("got %d findings", len(rep.Findings))
	}
	if rep.Findings[0].Func != "abc123" {
		t.Errorf("declaration SHA not recorded on the finding: %+v", rep.Findings[0])
	}
	if rep.Findings[0].Fingerprint == "" || rep.Findings[0].Suggest == "" {
		t.Errorf("finding is missing fingerprint or suggestion: %+v", rep.Findings[0])
	}
}
