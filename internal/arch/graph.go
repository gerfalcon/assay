package arch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

// Graph is a package dependency graph, keyed by module-relative path.
type Graph struct {
	// Module is the module path, e.g. example.com/shop.
	Module string
	// Edges maps a package to the packages it imports directly.
	Edges map[string][]string
	// Files maps a package to one representative source file, so a finding can
	// point somewhere a person can open.
	Files map[string]string
}

type goListPkg struct {
	ImportPath   string
	Dir          string
	Module       *struct{ Path, Dir string }
	GoFiles      []string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// LoadGoGraph builds the graph by invoking `go list` once for the whole module.
//
// Once, not per package: a repository with several hundred packages makes
// per-package invocation dominate the runtime, and history replay multiplies
// that by every commit.
//
// Only packages inside the module are kept. Stdlib and third-party code is not
// layered by this declaration, and including it would make every layer reach
// every other through `fmt`.
func LoadGoGraph(root string, includeTests bool) (*Graph, error) {
	// Ask for the main module EXPLICITLY. Inferring it from the first package
	// in a -deps stream that carries a Module.Path is wrong: -deps emits
	// dependencies before dependents, so on any repository with third-party
	// imports that first module is a library. Everything first-party then falls
	// outside "the module", the graph comes back nearly empty, and the check
	// reports "0 violations" — a silent pass, which is the one outcome this
	// tool exists to prevent.
	//
	// It appeared to work only on a dependency-free module, and then only by
	// luck: a first-party package importing nothing but stdlib happens to sort
	// before the libraries. One added import would have turned the check off
	// silently.
	modCmd := exec.Command("go", "list", "-m")
	modCmd.Dir = root
	modOut, modErr := modCmd.Output()
	if modErr != nil {
		return nil, fmt.Errorf("go list -m in %s: %w (not a Go module?)", root, modErr)
	}
	mainModule := strings.TrimSpace(string(modOut))
	if mainModule == "" || strings.Contains(mainModule, "\n") {
		return nil, fmt.Errorf("could not determine a single main module in %s: %q", root, mainModule)
	}

	cmd := exec.Command("go", "list", "-e", "-deps", "-json", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("go list in %s: %v%s", root, err, func() string {
			if msg == "" {
				return ""
			}
			return "\n" + msg
		}())
	}
	return parseGoList(strings.NewReader(string(out)), mainModule, includeTests)
}

func parseGoList(r io.Reader, mainModule string, includeTests bool) (*Graph, error) {
	g := &Graph{Module: mainModule, Edges: map[string][]string{}, Files: map[string]string{}}
	dec := json.NewDecoder(r)
	var pkgs []goListPkg

	for {
		var p goListPkg
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing go list output: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	if g.Module == "" {
		return nil, fmt.Errorf("no main module given")
	}

	inModule := func(ip string) bool {
		return ip == g.Module || strings.HasPrefix(ip, g.Module+"/")
	}

	for _, p := range pkgs {
		if !inModule(p.ImportPath) {
			continue
		}
		rel := relPath(g.Module, p.ImportPath)
		imports := p.Imports
		if includeTests {
			imports = append(append(append([]string{}, p.Imports...), p.TestImports...), p.XTestImports...)
		}
		seen := map[string]bool{}
		var deps []string
		for _, imp := range imports {
			if !inModule(imp) || imp == p.ImportPath {
				continue
			}
			d := relPath(g.Module, imp)
			if !seen[d] {
				seen[d] = true
				deps = append(deps, d)
			}
		}
		sort.Strings(deps) // deterministic traversal, so chains do not vary run to run
		g.Edges[rel] = deps
		if len(p.GoFiles) > 0 {
			g.Files[rel] = rel + "/" + p.GoFiles[0]
		}
	}
	return g, nil
}

func relPath(module, importPath string) string {
	if importPath == module {
		return "."
	}
	return strings.TrimPrefix(importPath, module+"/")
}

// Reachable returns the shortest dependency chain from src to any package for
// which stop returns true, or nil if none is reachable.
//
// THE CHAIN IS THE POINT. Reporting "domain violates infra" tells someone there
// is a problem; reporting "domain -> helper -> infra" tells them where to cut.
// Breadth-first so the chain reported is the shortest one, which is the one
// most likely to be the real coupling rather than an incidental long path.
func (g *Graph) Reachable(src string, stop func(pkg string) bool) []string {
	type node struct {
		pkg  string
		path []string
	}
	visited := map[string]bool{src: true}
	queue := []node{{pkg: src, path: []string{src}}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range g.Edges[cur.pkg] {
			if visited[dep] {
				continue
			}
			visited[dep] = true
			path := append(append([]string{}, cur.path...), dep)
			if stop(dep) {
				return path
			}
			queue = append(queue, node{pkg: dep, path: path})
		}
	}
	return nil
}

// Packages returns every package in the graph, sorted.
func (g *Graph) Packages() []string {
	out := make([]string, 0, len(g.Edges))
	for p := range g.Edges {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
