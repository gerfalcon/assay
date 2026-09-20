package main

import (
	"os"
	"strings"
	"testing"

	"github.com/sherzing/assay/cmd/internal/cmdtest"
)

func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }

// A repository with one obviously gnarly function and one tidy one, so "worst
// right now" has a right answer that can be asserted.
const fixtureSrc = `package core

// Tidy is short and flat.
func Tidy(a, b int) int { return a + b }

// Gnarly is the hotspot: deep nesting and many branches.
func Gnarly(xs []int, mode string) int {
	total := 0
	for _, x := range xs {
		if x > 0 {
			if mode == "double" {
				if x%2 == 0 {
					total += x * 2
				} else {
					total += x
				}
			} else if mode == "square" {
				for i := 0; i < x; i++ {
					if i%3 == 0 {
						total += i
					}
				}
			}
		}
	}
	return total
}
`

// measuresFrom runs a real scan and returns the JSONL, because lens's claim is
// that it reads a stream any conforming producer can write — the pipe is the
// thing under test as much as the rendering is.
func measuresFrom(t *testing.T) string {
	t.Helper()
	ratchet := cmdtest.Build(t, "ratchet")
	repo := cmdtest.Tree(t, map[string]string{"core/calc.go": fixtureSrc})
	return ratchet.Run(t, repo, "scan", ".", "--emit", "measures",
		"--repo", "fixture", "--org", "-").MustPass(t).Stdout
}

// lens ranks by the metric asked for and puts the worst first. Getting the
// order wrong makes the tool worse than useless: it sends people to fix the
// wrong function.
func TestTopRanksTheWorstFunctionFirst(t *testing.T) {
	lens := cmdtest.Build(t, "lens")
	dir := t.TempDir()

	r := lens.Pipe(t, dir, measuresFrom(t), "top", "--metric", "cognitive").MustPass(t)
	r.MustSay(t, "Gnarly", "median")
	gnarly := strings.Index(r.Stdout, "Gnarly")
	tidy := strings.Index(r.Stdout, "Tidy")
	if gnarly < 0 {
		t.Fatalf("the hotspot is missing from the ranking:\n%s", r)
	}
	if tidy >= 0 && tidy < gnarly {
		t.Errorf("the tidy function ranked above the gnarly one:\n%s", r)
	}

	// --plain drops the interpretation for scripting, and must drop all of it:
	// a half-plain output is neither readable nor parseable.
	plain := lens.Pipe(t, dir, measuresFrom(t), "top", "--metric", "cognitive", "--plain").MustPass(t)
	plain.MustSay(t, "Gnarly")
	plain.MustNotSay(t, "↳", "what to do")

	// --n caps the list.
	one := lens.Pipe(t, dir, measuresFrom(t), "top", "--metric", "cognitive", "--n", "1").MustPass(t)
	one.MustSay(t, "worst 1 by cognitive")
}

// Piping in a stream with no matching measures is an error, not an empty
// table. A silent blank is indistinguishable from "everything is fine".
func TestTopRefusesToInventAnAnswer(t *testing.T) {
	lens := cmdtest.Build(t, "lens")
	dir := t.TempDir()

	lens.Pipe(t, dir, measuresFrom(t), "top", "--metric", "no-such-metric").MustFail(t).
		MustSay(t, "no no-such-metric measures")

	// No stdin at all: say what to do rather than hanging on a terminal.
	lens.Run(t, dir, "top").MustFail(t).
		MustSay(t, "no input", "--store")
}

// A trend needs history, so this hands lens a series it could only have got
// from a store — which is the point: lens reads the format, not our store.
func TestTrendAndDiffReadAnyConformingStream(t *testing.T) {
	lens := cmdtest.Build(t, "lens")
	dir := t.TempDir()

	series := ""
	for _, p := range []struct {
		day string
		v   string
	}{
		{"2026-06-01", "30"}, {"2026-07-01", "26"},
		{"2026-08-01", "18"}, {"2026-09-01", "12"},
	} {
		series += `{"v":1,"kind":"measure","repo":"fixture","ts":"` + p.day +
			`T00:00:00Z","scope":"project","metric":"cognitive.p90","value":` + p.v + "}\n"
	}

	tr := lens.Pipe(t, dir, series, "trend", "--metric", "cognitive.p90", "--period", "month").MustPass(t)
	tr.MustSay(t, "fixture", "30", "12", "improving")

	// diff answers "what changed since", which is the question in a review.
	d := lens.Pipe(t, dir, series, "diff", "--since", "2026-07-15",
		"--metric", "cognitive.p90", "--scope", "project").MustPass(t)
	d.MustSay(t, "since 2026-07-15", "better")

	lens.Pipe(t, dir, series, "diff").MustFail(t).MustSay(t, "--since YYYY-MM-DD is required")
	lens.Pipe(t, dir, series, "diff", "--since", "last tuesday").MustFail(t).MustSay(t, "--since")

	cmp := lens.Pipe(t, dir, series, "compare", "--scope", "project").MustPass(t)
	cmp.MustSay(t, "fixture", "cognitive.p90")
}

// calibrate exists because shipped thresholds came from two codebases and are a
// poor universal truth. It must refuse when there is nothing to calibrate from
// rather than emitting bands derived from no data.
func TestCalibrateNeedsFunctionScopeData(t *testing.T) {
	lens := cmdtest.Build(t, "lens")
	dir := t.TempDir()

	c := lens.Pipe(t, dir, measuresFrom(t), "calibrate", "--lang", "go").MustPass(t)
	c.MustSay(t, "cognitive", "proposed bands", `"go": {`)

	projectOnly := `{"v":1,"kind":"measure","repo":"fixture","ts":"2026-09-01T00:00:00Z",` +
		`"scope":"project","metric":"cognitive.p90","value":12}` + "\n"
	lens.Pipe(t, dir, projectOnly, "calibrate").MustFail(t).
		MustSay(t, "no function-scope measures found")
}

func TestLensUsageAndVersion(t *testing.T) {
	lens := cmdtest.Build(t, "lens")
	dir := t.TempDir()
	lens.Run(t, dir, "version").MustPass(t).MustSay(t, "lens "+version)
	lens.Run(t, dir, "help").MustPass(t).MustSay(t, "read an assay stream at a glance")
	if got := lens.Run(t, dir, "microscope").MustFail(t).Code; got != 2 {
		t.Errorf("exit %d for an unknown command, want 2", got)
	}
	if got := lens.Run(t, dir).MustFail(t).Code; got != 2 {
		t.Errorf("exit %d for no arguments, want 2", got)
	}
}
