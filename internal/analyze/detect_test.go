package analyze

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Functional detection tests. Each directory under testdata/rules is named for
// the rule it exercises and holds two files:
//
//	bad.go   every line ending `// want` must produce a finding, and no other
//	         line may produce one
//	good.go  must produce no unjudged finding at all
//
// The good files are the more valuable half. They carry the shapes that have
// actually generated false positives on production code — variadic `...any`,
// `must*` panics, `//nolint` — so a rule that gets greedier fails here before it
// reaches anyone's repository.

const corpus = "testdata/rules"

// markedLines returns the 1-based line numbers whose trimmed text ends in the
// given marker.
func markedLines(t *testing.T, path, marker string) map[int]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	want := map[int]bool{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.HasSuffix(strings.TrimSpace(sc.Text()), marker) {
			want[n] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return want
}

// scanOne runs a single rule over a single file, by staging it alone in a temp
// directory. Staging keeps bad.go from polluting good.go's result and vice
// versa, since Scan works on directories.
func scanOne(t *testing.T, rule, src string) []findingAt {
	t.Helper()
	dir := t.TempDir()
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), body, 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Scan(dir, Options{Enabled: map[string]bool{rule: true}})
	if err != nil {
		t.Fatalf("scan %s: %v", src, err)
	}
	out := make([]findingAt, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		if f.Rule != rule {
			t.Errorf("%s: rule %q fired while only %q was enabled", src, f.Rule, rule)
			continue
		}
		out = append(out, findingAt{line: f.Line, verdict: f.Verdict, msg: f.Message})
	}
	return out
}

type findingAt struct {
	line    int
	verdict string
	msg     string
}

func TestDetectsBadExamples(t *testing.T) {
	for _, rule := range ruleDirs(t) {
		t.Run(rule, func(t *testing.T) {
			path := filepath.Join(corpus, rule, "bad.go")
			want := markedLines(t, path, "// want")
			if len(want) == 0 {
				t.Fatalf("%s has no `// want` markers — a bad file that asserts nothing", path)
			}

			got := map[int]bool{}
			for _, f := range scanOne(t, rule, path) {
				got[f.line] = true
				if f.msg == "" {
					t.Errorf("line %d: empty message — a finding nobody can act on", f.line)
				}
			}
			for line := range want {
				if !got[line] {
					t.Errorf("%s:%d marked `// want` but no finding was produced", path, line)
				}
			}
			for line := range got {
				if !want[line] {
					t.Errorf("%s:%d produced a finding that is not marked `// want`", path, line)
				}
			}
		})
	}
}

// The false-positive regression test. Every line in good.go is a shape a
// reasonable engineer writes on purpose.
func TestNoFindingsOnGoodExamples(t *testing.T) {
	for _, rule := range ruleDirs(t) {
		t.Run(rule, func(t *testing.T) {
			path := filepath.Join(corpus, rule, "good.go")
			// `// want-unjudged` marks a line that is expected to stay
			// unjudged — used to pin the annotation scope.
			expected := markedLines(t, path, "// want-unjudged")
			for _, f := range scanOne(t, rule, path) {
				// A finding carrying a verdict is the annotation mechanism
				// working, not a false positive.
				if f.verdict != "" || expected[f.line] {
					continue
				}
				t.Errorf("%s:%d false positive: %s", path, f.line, f.msg)
			}
		})
	}
}

// Every rule in the registry needs a corpus directory. Without this a rule can
// be added with no detection test and nobody notices.
func TestEveryRuleHasACorpus(t *testing.T) {
	have := map[string]bool{}
	for _, d := range ruleDirs(t) {
		have[d] = true
	}
	for rule := range Rules {
		if !have[rule] {
			t.Errorf("rule %q has no testdata/rules/%s directory with good.go and bad.go", rule, rule)
		}
	}
	for d := range have {
		if _, ok := Rules[d]; !ok {
			t.Errorf("testdata/rules/%s does not correspond to any registered rule", d)
		}
	}
}

// The corpus must contain at least one annotated finding, or the verdict path
// in TestNoFindingsOnGoodExamples is never exercised and the `continue` above
// silently becomes a hole that hides real false positives.
func TestCorpusExercisesTheVerdictPath(t *testing.T) {
	judged := 0
	for _, rule := range ruleDirs(t) {
		for _, f := range scanOne(t, rule, filepath.Join(corpus, rule, "good.go")) {
			if f.verdict != "" {
				judged++
			}
		}
	}
	if judged == 0 {
		t.Error("no good.go carries a `// quality:` annotation, so the verdict " +
			"exemption in TestNoFindingsOnGoodExamples is untested")
	}
}

func ruleDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, f := range []string{"good.go", "bad.go"} {
			if _, err := os.Stat(filepath.Join(corpus, e.Name(), f)); err != nil {
				t.Errorf("%s/%s is missing: every rule needs both a positive and a negative case", e.Name(), f)
			}
		}
		dirs = append(dirs, e.Name())
	}
	if len(dirs) == 0 {
		t.Fatal("corpus is empty")
	}
	return dirs
}
