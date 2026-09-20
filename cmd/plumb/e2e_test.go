package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sherzing/assay/cmd/internal/cmdtest"
)

// End-to-end against real Go modules on disk, driving the real binary. The
// unit tests use a synthetic graph; these prove the `go list` integration and
// the exit codes, which is where a checker actually gets wired into CI.

func plumb(t *testing.T) cmdtest.Bin {
	t.Helper()
	return cmdtest.Build(t, "plumb")
}

const declClean = "# Architecture\n\n" +
	"Dependencies point inward. The domain must be testable with no database.\n\n" +
	"```arch\n" +
	"layer domain   internal/domain\n" +
	"layer infra    internal/impl\n" +
	"forbid domain -> infra\n" +
	"```\n\n" +
	"## Why\n\nKeeping the domain pure is what makes evaluation order analysable.\n"

// fixture writes a module whose domain package imports the given packages.
func fixture(t *testing.T, domainImports ...string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/shop\n\ngo 1.22\n")
	write("ARCHITECTURE.md", declClean)
	write("internal/impl/db.go", "package impl\n\nfunc Query() string { return \"row\" }\n")
	write("internal/helper/helper.go",
		"package helper\n\nimport \"example.com/shop/internal/impl\"\n\nfunc Fetch() string { return impl.Query() }\n")

	var imports, body strings.Builder
	for i, imp := range domainImports {
		imports.WriteString("\t\"example.com/shop/" + imp + "\"\n")
		name := imp[strings.LastIndex(imp, "/")+1:]
		switch name {
		case "impl":
			body.WriteString("func UseImpl() string { return impl.Query() }\n")
		case "helper":
			body.WriteString("func UseHelper() string { return helper.Fetch() }\n")
		}
		_ = i
	}
	src := "package domain\n"
	if imports.Len() > 0 {
		src += "\nimport (\n" + imports.String() + ")\n\n" + body.String()
	} else {
		src += "\nfunc Evaluate(n int) int { return n * 2 }\n"
	}
	write("internal/domain/domain.go", src)
	return dir
}

// THE LOAD-BEARING TEST, end to end. A direct-import checker passes the second
// case. plumb must not.
func TestIndirectViolationFailsTheGate(t *testing.T) {
	bin := plumb(t)

	t.Run("direct", func(t *testing.T) {
		dir := fixture(t, "internal/impl")
		r := bin.Run(t, dir, "scan", ".").MustPass(t)
		r.MustSay(t, "1 violations", "internal/domain → internal/impl")
	})

	t.Run("one hop", func(t *testing.T) {
		dir := fixture(t, "internal/helper")
		r := bin.Run(t, dir, "scan", ".").MustPass(t)
		r.MustSay(t, "1 violations", "internal/domain → internal/helper → internal/impl")
	})

	t.Run("clean", func(t *testing.T) {
		dir := fixture(t)
		bin.Run(t, dir, "scan", ".").MustPass(t).MustSay(t, "0 violations")
	})
}

// The ratchet contract: adopt on a dirty codebase, then only new violations
// fail. Without this a team turns the rule on, sees forty failures, and turns
// it off the same afternoon.
func TestRatchetToleratesTheOldAndFailsTheNew(t *testing.T) {
	bin := plumb(t)
	dir := fixture(t, "internal/impl") // one existing violation

	bin.Run(t, dir, "baseline", ".").MustPass(t).MustSay(t, "1 tolerated")
	bin.Run(t, dir, "check", ".").MustPass(t).MustSay(t, "1 tolerated, 0 new")

	// A second domain package picks up the forbidden dependency.
	sub := filepath.Join(dir, "internal/domain/pricing")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "p.go"),
		[]byte("package pricing\n\nimport \"example.com/shop/internal/helper\"\n\nfunc P() string { return helper.Fetch() }\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	r := bin.Run(t, dir, "check", ".").MustFail(t)
	r.MustSay(t, "1 new", "internal/domain/pricing")
	if strings.Count(r.Stdout, "layer-violation") != 1 {
		t.Errorf("check should name only the NEW violation:\n%s", r.Stdout)
	}

	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	bin.Run(t, dir, "check", ".").MustPass(t)
}

// Without this guard "fix the failure" becomes "rewrite the baseline" and the
// gate quietly stops meaning anything.
func TestBaselineCannotBeQuietlyRegenerated(t *testing.T) {
	bin := plumb(t)
	dir := fixture(t, "internal/impl")
	bin.Run(t, dir, "baseline", ".").MustPass(t)

	path := filepath.Join(dir, ".plumb-baseline.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	bin.Run(t, dir, "baseline", ".").MustFail(t).MustSay(t, "already exists", "silently forgive")

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the refused regeneration still modified the baseline")
	}
	bin.Run(t, dir, "baseline", ".", "--force").MustPass(t)
}

// A missing or empty declaration must be an error. Passing because there is
// nothing to check is the failure mode that makes the whole tool a placebo.
func TestNoDeclarationIsAnError(t *testing.T) {
	bin := plumb(t)

	dir := fixture(t)
	if err := os.Remove(filepath.Join(dir, "ARCHITECTURE.md")); err != nil {
		t.Fatal(err)
	}
	bin.Run(t, dir, "scan", ".").MustFail(t).MustSay(t, "ARCHITECTURE.md")

	dir2 := fixture(t)
	if err := os.WriteFile(filepath.Join(dir2, "ARCHITECTURE.md"),
		[]byte("# Architecture\n\nWe are layered, honest.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin.Run(t, dir2, "scan", ".").MustFail(t).MustSay(t, "no ```arch block")
}

func TestCheckWithoutABaselineFails(t *testing.T) {
	dir := fixture(t, "internal/impl")
	plumb(t).Run(t, dir, "check", ".").MustFail(t).MustSay(t, "no baseline", "plumb baseline")
}

// The original ratchet bug: flags after a positional were silently dropped.
func TestFlagsWorkOnEitherSideOfThePath(t *testing.T) {
	bin := plumb(t)
	dir := fixture(t, "internal/impl")
	if err := os.Rename(filepath.Join(dir, "ARCHITECTURE.md"), filepath.Join(dir, "ARCH.md")); err != nil {
		t.Fatal(err)
	}
	before := bin.Run(t, dir, "scan", "--doc", "ARCH.md", ".").MustPass(t)
	after := bin.Run(t, dir, "scan", ".", "--doc", "ARCH.md").MustPass(t)
	if before.Stdout != after.Stdout {
		t.Errorf("flag position changed the result — it was silently dropped\nbefore:\n%s\nafter:\n%s",
			before.Stdout, after.Stdout)
	}
}

func TestUnknownFlagAndCommandAreErrors(t *testing.T) {
	bin := plumb(t)
	dir := fixture(t)
	bin.Run(t, dir, "scan", ".", "--nope").MustFail(t)
	bin.Run(t, dir, "wat", ".").MustFail(t).MustSay(t, "unknown command")
	bin.Run(t, dir, "scan", ".", "..").MustFail(t).MustSay(t, "expected one directory")
}

// The JSONL contract: plumb output must load into strata like any other tool's.
func TestEmitFindingsLoadIntoStrata(t *testing.T) {
	dir := fixture(t, "internal/helper")
	out := plumb(t).Run(t, dir, "scan", ".", "--emit", "findings").MustPass(t)
	if !strings.Contains(out.Stdout, `"kind":"finding"`) || !strings.Contains(out.Stdout, `"tool":"plumb"`) {
		t.Fatalf("not a conforming finding stream:\n%s", out.Stdout)
	}

	strata := cmdtest.Build(t, "strata")
	store := filepath.Join(t.TempDir(), "store")
	strata.Pipe(t, dir, out.Stdout, "append", "--store", store).MustPass(t).MustSay(t, "1")

	// `strata stat` reports measures and verdicts but not findings, so read the
	// partition directly rather than assert on a number it does not print.
	var landed int
	err := filepath.Walk(filepath.Join(store, "findings"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		landed += strings.Count(string(b), `"tool":"plumb"`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if landed != 1 {
		t.Errorf("%d plumb findings reached the store, want 1", landed)
	}
}

// Tightening must be free, or the change process gets routed around.
func TestDiffClassifiesTighteningAndLoosening(t *testing.T) {
	bin := plumb(t)
	dir := fixture(t)
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), cmdtest.GitEnv()...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", ".")
	git("add", "-A")
	git("commit", "-qm", "base")
	git("branch", "-f", "baseref")

	doc := filepath.Join(dir, "ARCHITECTURE.md")
	decl := func(block string) string {
		return "# Architecture\n\n```arch\n" + block + "```\n\n## Why\n\nBecause.\n"
	}
	put := func(block string) {
		t.Helper()
		if err := os.WriteFile(doc, []byte(decl(block)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Adding a rule forbids strictly more: free.
	put("layer domain internal/domain\nlayer infra internal/impl\nlayer svc internal/service\n" +
		"forbid domain -> infra\nforbid svc -> infra\n")
	bin.Run(t, dir, "diff", ".", "--base", "baseref").MustPass(t).
		MustSay(t, "tightening").MustNotSay(t, "second reviewer")

	// Reordering is cosmetic and must not demand a reviewer, or the process
	// gets ignored for the changes that matter.
	put("layer infra internal/impl\nlayer domain internal/domain\nforbid domain -> infra\n")
	bin.Run(t, dir, "diff", ".", "--base", "baseref").MustPass(t).MustSay(t, "no change")

	// Narrowing a layer releases packages from the rule: loosening, even though
	// the file got no shorter.
	put("layer domain internal/domain\nlayer infra internal/impl\nforbid domain -> infra\n")
	bin.Run(t, dir, "diff", ".", "--base", "baseref").MustPass(t).MustSay(t, "no change")

	put("layer domain internal/domain\nlayer infra internal/impl internal/helper\nforbid infra -> domain\n")
	bin.Run(t, dir, "diff", ".", "--base", "baseref").MustFail(t).MustSay(t, "second reviewer")
}
