package dcm

import (
	"strconv"
	"strings"
	"testing"
)

// The reason this importer reads source: a fingerprint keyed on the reported
// line would turn every reformat into a wave of new violations.
func TestFingerprintsFollowTheCodeNotTheLine(t *testing.T) {
	same := imp(t, 0, Options{Root: setup(t, 0)})
	moved := imp(t, 2, Options{Root: setup(t, 2)})
	for _, a := range same.Findings {
		b := byRule(t, moved, a.Rule)
		if a.Fingerprint != b.Fingerprint {
			t.Errorf("%s: fingerprint changed when the code moved: %s -> %s", a.Rule, a.Fingerprint, b.Fingerprint)
		}
		if b.Line != a.Line+2 && a.Line != 0 {
			t.Errorf("%s: line = %d, want %d", a.Rule, b.Line, a.Line+2)
		}
	}

	// Identity is the region DCM reported — here the `dynamic` token — so a
	// rename outside it keeps the fingerprint, and an edit inside it does not.
	root := setup(t, 0)
	write(t, root, "lib/a.dart", strings.Replace(dartSrc, "dynamic bag", "dynamic sack", 1))
	renamed := imp(t, 0, Options{Root: root})
	if byRule(t, same, "dcm:avoid-dynamic").Fingerprint != byRule(t, renamed, "dcm:avoid-dynamic").Fingerprint {
		t.Error("renaming the variable next to the offending span changed the fingerprint")
	}
	write(t, root, "lib/a.dart", strings.Replace(dartSrc, "dynamic bag", "Dynamic bag", 1))
	edited := imp(t, 0, Options{Root: root})
	if byRule(t, same, "dcm:avoid-dynamic").Fingerprint == byRule(t, edited, "dcm:avoid-dynamic").Fingerprint {
		t.Error("a different offending span produced the same fingerprint")
	}
}

// Identical findings in one function are numbered, so the ratchet tracks how
// many there are without ever depending on a line number.
func TestIdenticalFindingsGetOrdinals(t *testing.T) {
	root := t.TempDir()
	write(t, root, "lib/a.dart", "class Foo {\n  dynamic a;\n  dynamic b;\n  dynamic c;\n}\n")
	doc := func(lines ...int) string {
		var issues []string
		for _, l := range lines {
			issues = append(issues, `{"id":"avoid-dynamic","message":"m","location":{"startLine":`+strconv.Itoa(l)+`,"startColumn":3,"endLine":`+strconv.Itoa(l)+`,"endColumn":10},"severity":"warning"}`)
		}
		return `{"formatVersion":13,"analyzeResults":[{"path":"lib/a.dart","issues":[` + strings.Join(issues, ",") + `]}]}`
	}
	three, err := Import(strings.NewReader(doc(2, 3, 4)), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range three.Findings {
		if seen[f.Fingerprint] {
			t.Fatalf("identical findings still share a fingerprint: %+v", three.Findings)
		}
		seen[f.Fingerprint] = true
	}
	two, err := Import(strings.NewReader(doc(2, 3)), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// Dropping the last instance keeps the first two identities: one "fixed".
	for _, f := range two.Findings {
		if !seen[f.Fingerprint] {
			t.Errorf("fingerprint %s changed when the third instance was removed", f.Fingerprint)
		}
	}
	if len(two.Findings) != 2 || two.Findings[0].Fingerprint == two.Findings[1].Fingerprint {
		t.Errorf("two instances = %+v", two.Findings)
	}
}

// A rule about the whole file, such as avoid-long-files, is reported at 1:1
// spanning to the end. Its identity must be the file, not the first line's
// text, or a new header comment reads as one finding fixed and one new.
func TestFileLevelFindingsSurviveHeaderChanges(t *testing.T) {
	doc := func(last int) string {
		return `{"formatVersion":13,"analyzeResults":[{"path":"lib/a.dart","issues":[{"id":"avoid-long-files","message":"Avoid long files.","location":{"startLine":1,"startColumn":1,"endLine":` + strconv.Itoa(last) + `,"endColumn":1},"severity":"warning"}]}]}`
	}
	root := t.TempDir()
	write(t, root, "lib/a.dart", dartSrc)
	before, err := Import(strings.NewReader(doc(15)), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, "lib/a.dart", "// a new header comment\n"+dartSrc)
	after, err := Import(strings.NewReader(doc(16)), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if before.Findings[0].Fingerprint != after.Findings[0].Fingerprint {
		t.Error("a file-level finding changed identity when the file's first line changed")
	}
}
