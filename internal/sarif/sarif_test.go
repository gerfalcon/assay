package sarif

import (
	"strings"
	"testing"

	"github.com/sherzing/assay/internal/model"
)

func imp(t *testing.T, js string, opt Options) []model.Finding {
	t.Helper()
	fs, err := Import(strings.NewReader(js), opt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return fs
}

const basic = `{"version":"2.1.0","runs":[{
 "tool":{"driver":{"name":"testlint","rules":[
   {"id":"R1","shortDescription":{"text":"rule one"},"defaultConfiguration":{"level":"error"}}]}},
 "results":[
  {"ruleId":"R1","message":{"text":"boom"},
   "locations":[{"physicalLocation":{"artifactLocation":{"uri":"src/a.go"},
     "region":{"startLine":10,"startColumn":3,"snippet":{"text":"x := y.(int)"}}}}]}]}]}`

func TestImportBasic(t *testing.T) {
	fs := imp(t, basic, Options{})
	if len(fs) != 1 {
		t.Fatalf("got %d findings, want 1", len(fs))
	}
	f := fs[0]
	if f.Rule != "testlint:R1" {
		t.Errorf("rule = %q, want testlint:R1 (tool-namespaced)", f.Rule)
	}
	if f.File != "src/a.go" || f.Line != 10 || f.Col != 3 {
		t.Errorf("location = %s:%d:%d, want src/a.go:10:3", f.File, f.Line, f.Col)
	}
	if f.Severity != model.Error {
		t.Errorf("severity = %q, want error (from rule defaultConfiguration)", f.Severity)
	}
	if f.Fingerprint == "" {
		t.Error("no fingerprint generated")
	}
}

// Two tools emitting the same rule ID must not collide in one baseline, or each
// silently tolerates the other's violations.
func TestRuleIDsAreNamespacedByTool(t *testing.T) {
	mk := func(tool string) string {
		return `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"` + tool + `"}},
		 "results":[{"ruleId":"unused","message":{"text":"m"},
		  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}]}]}`
	}
	a := imp(t, mk("golangci-lint"), Options{})
	b := imp(t, mk("roslyn"), Options{})
	if a[0].Rule == b[0].Rule {
		t.Fatalf("rules collided across tools: both %q", a[0].Rule)
	}
	if a[0].Fingerprint == b[0].Fingerprint {
		t.Error("fingerprints collided across tools — one tool's baseline would excuse the other's findings")
	}
}

// The same reason native findings exclude the line number: a reformat must not
// read as a wave of new violations.
func TestFingerprintSurvivesLineMovement(t *testing.T) {
	mk := func(line int) string {
		return `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
		 "results":[{"ruleId":"R","message":{"text":"same defect"},
		  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},
		   "region":{"startLine":` + itoa(line) + `,"snippet":{"text":"foo(bar)"}}}}]}]}]}`
	}
	a := imp(t, mk(10), Options{})
	b := imp(t, mk(90), Options{})
	if a[0].Line == b[0].Line {
		t.Fatal("test not exercising line movement")
	}
	if a[0].Fingerprint != b[0].Fingerprint {
		t.Errorf("fingerprint changed on line movement alone: %s vs %s",
			a[0].Fingerprint, b[0].Fingerprint)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A producer that supplies its own stable fingerprint knows better than we do.
func TestProducerFingerprintPreferred(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"codeql"}},
	 "results":[{"ruleId":"R","message":{"text":"m"},
	  "partialFingerprints":{"primaryLocationLineHash":"deadbeefcafe0123456789"},
	  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}]}]}`
	f := imp(t, js, Options{})[0]
	if f.Fingerprint != "deadbeefcafe0123" {
		t.Errorf("fingerprint = %q, want the producer's primaryLocationLineHash", f.Fingerprint)
	}
}

// Honouring suppressions is the same principle as honouring //nolint: a tool
// that cannot be told no gets switched off entirely.
func TestSuppressionsHonoured(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
	 "results":[{"ruleId":"R","message":{"text":"m"},
	  "suppressions":[{"kind":"inSource","justification":"reviewed"}],
	  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}]}]}`
	if got := len(imp(t, js, Options{})); got != 0 {
		t.Errorf("suppressed result imported by default: got %d, want 0", got)
	}
	if got := len(imp(t, js, Options{IncludeSuppressed: true})); got != 1 {
		t.Errorf("--include-suppressed did not import it: got %d, want 1", got)
	}
}

func TestAbsolutePathsMadeRelative(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
	 "results":[{"ruleId":"R","message":{"text":"m"},
	  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"file:///repo/src/a.cs"},"region":{"startLine":1}}}]}]}]}`
	f := imp(t, js, Options{Root: "/repo"})[0]
	if f.File != "src/a.cs" {
		t.Errorf("file = %q, want src/a.cs — imported findings must key like native ones", f.File)
	}
}

func TestSeverityMapping(t *testing.T) {
	cases := map[string]model.Severity{
		"error": model.Error, "warning": model.Warn,
		"note": model.Info, "none": model.Info,
		"": model.Warn, // SARIF's documented default
	}
	for level, want := range cases {
		js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
		 "results":[{"ruleId":"R","level":"` + level + `","message":{"text":"m"},
		  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":1}}}]}]}]}`
		if got := imp(t, js, Options{})[0].Severity; got != want {
			t.Errorf("level %q -> %q, want %q", level, got, want)
		}
	}
}

// A finding nobody can locate is not actionable, and cannot be fingerprinted
// stably. Dropping it beats carrying it.
func TestResultWithoutLocationDropped(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
	 "results":[{"ruleId":"R","message":{"text":"no location"}}]}]}`
	if got := len(imp(t, js, Options{})); got != 0 {
		t.Errorf("got %d findings, want 0", got)
	}
}

func TestLogicalLocationBecomesFunc(t *testing.T) {
	js := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},
	 "results":[{"ruleId":"R","message":{"text":"m"},
	  "locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.cs"},"region":{"startLine":1}},
	   "logicalLocations":[{"name":"Get","fullyQualifiedName":"Api.Controller.Get","kind":"function"}]}]}]}]}`
	if got := imp(t, js, Options{})[0].Func; got != "Api.Controller.Get" {
		t.Errorf("func = %q, want the fully qualified logical location", got)
	}
}

func TestEmptyAndMalformed(t *testing.T) {
	if fs := imp(t, `{"version":"2.1.0","runs":[]}`, Options{}); len(fs) != 0 {
		t.Errorf("empty runs gave %d findings", len(fs))
	}
	if _, err := Import(strings.NewReader("not json"), Options{}); err == nil {
		t.Error("malformed SARIF should error, not silently produce nothing")
	}
}
