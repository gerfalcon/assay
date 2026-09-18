package analyze

import (
	"go/ast"
	"go/token"
)

// cyclomatic counts independent paths: one, plus one per branch point.
//
// Counts if, for, range, comm clauses, non-default case clauses, and each
// && / || operand beyond the first. Deliberately does NOT count else — an
// else adds no new path, it is the other side of a branch already counted.
func cyclomatic(fn ast.Node) int {
	n := 1
	ast.Inspect(fn, func(node ast.Node) bool {
		switch s := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.CommClause:
			n++
		case *ast.CaseClause:
			// A default clause carries no condition, so it adds no path.
			if len(s.List) > 0 {
				n++
			}
		case *ast.BinaryExpr:
			if s.Op == token.LAND || s.Op == token.LOR {
				n++
			}
		}
		return true
	})
	return n
}

// cognitive implements Campbell's cognitive complexity, which is not a
// rename of cyclomatic complexity: it models how hard code is for a human to
// follow rather than how many paths it has.
//
// Three rules drive the difference:
//   - nesting is penalised — a branch three levels deep costs more than one at
//     the top, because the reader has to hold the enclosing conditions in mind
//   - else and else-if cost a flat 1 with no nesting penalty, since they
//     continue a structure the reader has already paid for
//   - a run of the same logical operator costs 1 for the whole run, not one per
//     operand; `a && b && c` is one idea, `a && b || c` is two
//
// This is why a long flat switch scores low here and high on cyclomatic: many
// paths, but nothing to hold in your head.
func cognitive(fn *ast.FuncDecl) int {
	c := &cogWalker{}
	if fn.Body != nil {
		c.block(fn.Body, 0)
	}
	return c.score
}

type cogWalker struct{ score int }

func (c *cogWalker) inc(nesting int) { c.score += 1 + nesting }
func (c *cogWalker) flat()           { c.score++ }

func (c *cogWalker) block(b *ast.BlockStmt, nesting int) {
	if b == nil {
		return
	}
	for _, s := range b.List {
		c.stmt(s, nesting)
	}
}

func (c *cogWalker) stmt(s ast.Stmt, nesting int) {
	switch n := s.(type) {
	case *ast.IfStmt:
		c.inc(nesting)
		c.logicalRuns(n.Cond)
		c.block(n.Body, nesting+1)
		c.elseChain(n.Else, nesting)

	case *ast.ForStmt:
		c.inc(nesting)
		c.logicalRuns(n.Cond)
		c.block(n.Body, nesting+1)

	case *ast.RangeStmt:
		c.inc(nesting)
		c.block(n.Body, nesting+1)

	case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
		c.inc(nesting)
		if sw, ok := s.(*ast.SwitchStmt); ok {
			c.logicalRuns(sw.Tag)
		}
		body, _ := clauseBodies(s)
		for _, st := range body {
			c.stmt(st, nesting+1)
		}

	case *ast.BlockStmt:
		c.block(n, nesting)

	case *ast.LabeledStmt:
		c.stmt(n.Stmt, nesting)

	case *ast.BranchStmt:
		// goto and labelled break/continue are jumps the reader must chase.
		// A bare break or continue is ordinary loop vocabulary; don't tax it.
		if n.Label != nil || n.Tok == token.GOTO {
			c.flat()
		}

	case *ast.DeferStmt, *ast.GoStmt:
		// Deliberately free: idiomatic Go, and taxing defer would push people
		// towards manual cleanup, which is worse code.

	default:
		// Function literals nest: their body is read inside the current context.
		inlineFuncLits(s, func(b *ast.BlockStmt) { c.block(b, nesting+1) })
	}
}

// elseChain handles else / else-if. Both cost a flat 1: the reader is
// continuing a structure they already paid the nesting price for.
func (c *cogWalker) elseChain(e ast.Stmt, nesting int) {
	switch n := e.(type) {
	case nil:
		return
	case *ast.IfStmt: // else if
		c.flat()
		c.logicalRuns(n.Cond)
		c.block(n.Body, nesting+1)
		c.elseChain(n.Else, nesting)
	case *ast.BlockStmt: // plain else
		c.flat()
		c.block(n, nesting+1)
	}
}

// logicalRuns charges 1 per *run* of a logical operator rather than per
// operand, so `a && b && c` costs 1 and `a && b || c` costs 2.
func (c *cogWalker) logicalRuns(e ast.Expr) {
	if e == nil {
		return
	}
	var last token.Token
	var walk func(ast.Expr)
	walk = func(x ast.Expr) {
		be, ok := x.(*ast.BinaryExpr)
		if !ok {
			if p, ok := x.(*ast.ParenExpr); ok {
				saved := last
				last = token.ILLEGAL // parens restart the run
				walk(p.X)
				last = saved
			}
			return
		}
		if be.Op == token.LAND || be.Op == token.LOR {
			if be.Op != last {
				c.flat()
				last = be.Op
			}
			walk(be.X)
			walk(be.Y)
			return
		}
		walk(be.X)
		walk(be.Y)
	}
	walk(e)
}

// clauseBodies flattens the statement bodies of a switch, type switch or
// select. All three share the shape "header, then a list of clauses, each
// holding statements one level deeper", and writing that out three times in
// every walker is what made this file's own complexity score the worst in the
// codebase. Extracted after ratchet reported it.
func clauseBodies(s ast.Stmt) ([]ast.Stmt, bool) {
	var clauses []ast.Stmt
	switch n := s.(type) {
	case *ast.SwitchStmt:
		clauses = n.Body.List
	case *ast.TypeSwitchStmt:
		clauses = n.Body.List
	case *ast.SelectStmt:
		clauses = n.Body.List
	default:
		return nil, false
	}
	var out []ast.Stmt
	for _, cl := range clauses {
		switch c := cl.(type) {
		case *ast.CaseClause:
			out = append(out, c.Body...)
		case *ast.CommClause:
			out = append(out, c.Body...)
		}
	}
	return out, true
}

// inlineFuncLits walks function literals inside a statement at the given depth.
func inlineFuncLits(s ast.Stmt, visit func(*ast.BlockStmt)) {
	ast.Inspect(s, func(node ast.Node) bool {
		if fl, ok := node.(*ast.FuncLit); ok {
			visit(fl.Body)
			return false
		}
		return true
	})
}

// maxNesting reports the deepest block nesting inside a function. Cheap to
// compute, easy to explain in review, and a good early warning: nesting
// usually rises before complexity scores do.
func maxNesting(fn *ast.FuncDecl) int {
	if fn.Body == nil {
		return 0
	}
	w := &nestWalker{}
	w.block(fn.Body, 0)
	return w.deepest
}

type nestWalker struct{ deepest int }

func (w *nestWalker) mark(d int) {
	if d > w.deepest {
		w.deepest = d
	}
}

func (w *nestWalker) block(b *ast.BlockStmt, d int) {
	if b == nil {
		return
	}
	for _, s := range b.List {
		w.stmt(s, d)
	}
}

func (w *nestWalker) stmt(s ast.Stmt, d int) {
	switch n := s.(type) {
	case *ast.IfStmt:
		w.mark(d + 1)
		w.block(n.Body, d+1)
		// else does not deepen: it is the other arm of the same branch.
		w.stmt(n.Else, d)
	case *ast.ForStmt:
		w.mark(d + 1)
		w.block(n.Body, d+1)
	case *ast.RangeStmt:
		w.mark(d + 1)
		w.block(n.Body, d+1)
	case *ast.BlockStmt:
		w.mark(d + 1)
		w.block(n, d+1)
	case *ast.LabeledStmt:
		w.stmt(n.Stmt, d)
	case nil:
		return
	default:
		if body, ok := clauseBodies(s); ok {
			w.mark(d + 1)
			for _, st := range body {
				w.stmt(st, d+1)
			}
			return
		}
		inlineFuncLits(s, func(b *ast.BlockStmt) { w.block(b, d) })
	}
}

// countStatements counts ast.Stmt nodes rather than lines. Line counts move
// when someone reformats or adds comments; statement counts only move when the
// code actually changes, which is what we want on a trend line.
func countStatements(fn ast.Node) int {
	n := 0
	ast.Inspect(fn, func(node ast.Node) bool {
		switch node.(type) {
		case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause, nil:
			// containers, not statements in their own right
		default:
			if _, ok := node.(ast.Stmt); ok {
				n++
			}
		}
		return true
	})
	return n
}

func fieldCount(fl *ast.FieldList) int {
	if fl == nil {
		return 0
	}
	n := 0
	for _, f := range fl.List {
		if len(f.Names) == 0 {
			n++ // unnamed result
			continue
		}
		n += len(f.Names)
	}
	return n
}
