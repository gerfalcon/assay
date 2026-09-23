package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sherzing/assay/internal/model"
)

func sample() *model.Report {
	r := &model.Report{
		Root: "/x",
		Funcs: []model.FuncMetrics{
			{File: "a.go", Line: 10, Name: "Simple", Exported: true, Cyclomatic: 1, Cognitive: 0, MaxNesting: 1, Statements: 3},
			{File: "b.go", Line: 20, Name: "Gnarly", Recv: "Server", Cyclomatic: 9, Cognitive: 21, MaxNesting: 4, Statements: 40},
		},
		Findings: []model.Finding{
			{Rule: "panic-in-library", Severity: model.Warn, File: "b.go", Line: 22, Col: 2,
				Message: "panic in library code", Suggest: "return an error", Fingerprint: "f1"},
			{Rule: "error-swallowed", Severity: model.Error, File: "a.go", Line: 11, Col: 4,
				Message: "error discarded", Fingerprint: "f2"},
		},
	}
	r.Summarise(2)
	return r
}

// THE LOAD-BEARING TEST for CSV.
//
// A path containing a comma used to shift every subsequent column, silently.
// A spreadsheet that is confidently wrong is worse than one that fails to open,
// because nobody re-checks a number that parsed.
func TestCSVQuotesFieldsContainingSeparators(t *testing.T) {
	r := &model.Report{Funcs: []model.FuncMetrics{
		{File: "pkg/a,b/handler.go", Line: 1, Name: "Serve", Cyclomatic: 2},
		{File: `pkg/"quoted"/x.go`, Line: 2, Name: "Also,Comma", Cyclomatic: 3},
		{File: "pkg/multi\nline.go", Line: 3, Name: "Weird", Cyclomatic: 4},
	}}

	var buf bytes.Buffer
	if err := CSV(&buf, r); err != nil {
		t.Fatal(err)
	}

	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("output is not parseable CSV: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want header + 3", len(rows))
	}
	for i, row := range rows {
		if len(row) != 11 {
			t.Errorf("row %d has %d columns, want 11 — a separator in a value shifted the row", i, len(row))
		}
	}
	if rows[1][0] != "pkg/a,b/handler.go" {
		t.Errorf("path round-trip: got %q, want the comma preserved inside one field", rows[1][0])
	}
	if rows[2][2] != "Also,Comma" {
		t.Errorf("func name round-trip: got %q", rows[2][2])
	}
	if rows[3][0] != "pkg/multi\nline.go" {
		t.Errorf("embedded newline round-trip: got %q", rows[3][0])
	}
}

func TestCSVHeaderAndValues(t *testing.T) {
	var buf bytes.Buffer
	if err := CSV(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := []string{"file", "line", "func", "recv", "exported",
		"cyclomatic", "cognitive", "maxNesting", "statements", "params", "results"}
	for i, h := range wantHeader {
		if rows[0][i] != h {
			t.Errorf("header column %d = %q, want %q", i, rows[0][i], h)
		}
	}
	if rows[1][2] != "Simple" || rows[1][4] != "true" {
		t.Errorf("row 1 = %v", rows[1])
	}
	if rows[2][3] != "Server" || rows[2][4] != "false" {
		t.Errorf("row 2 = %v", rows[2])
	}
}

// Ranking by cognitive complexity means the hotspot list points at what is hard
// to READ, which is what you send a person to fix. Ranking by cyclomatic would
// promote long flat switch statements nobody struggles with.
func TestTextRanksHotspotsByCognitiveComplexity(t *testing.T) {
	var buf bytes.Buffer
	if err := Text(&buf, sample(), 10); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	gnarly, simple := strings.Index(out, "Gnarly"), strings.Index(out, "Simple")
	if gnarly < 0 || simple < 0 {
		t.Fatalf("both functions should be listed:\n%s", out)
	}
	if gnarly > simple {
		t.Errorf("Gnarly (cog 21) listed after Simple (cog 0):\n%s", out)
	}
	if !strings.Contains(out, "Server.Gnarly") {
		t.Errorf("method not qualified by its receiver:\n%s", out)
	}
}

func TestTextCapsTheHotspotList(t *testing.T) {
	r := &model.Report{}
	for i := 0; i < 50; i++ {
		r.Funcs = append(r.Funcs, model.FuncMetrics{File: "a.go", Name: "f", Cognitive: i})
	}
	r.Summarise(1)

	var buf bytes.Buffer
	if err := Text(&buf, r, 5); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "top 5 by cognitive") {
		t.Errorf("list not capped at 5:\n%s", buf.String())
	}

	buf.Reset()
	if err := Text(&buf, r, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "top ") {
		t.Error("top=0 should suppress the hotspot list entirely")
	}
}

// Text must not reorder the report's own slice: the caller goes on to write
// JSON from the same struct, and a reordered Funcs would make the two outputs
// of one scan disagree.
func TestTextDoesNotReorderTheReport(t *testing.T) {
	r := sample()
	first := r.Funcs[0].Name
	var buf bytes.Buffer
	if err := Text(&buf, r, 10); err != nil {
		t.Fatal(err)
	}
	if r.Funcs[0].Name != first {
		t.Errorf("Funcs was sorted in place: now starts with %q, was %q", r.Funcs[0].Name, first)
	}
}

func TestTextSaysNoFindingsRatherThanPrintingNothing(t *testing.T) {
	r := &model.Report{Funcs: []model.FuncMetrics{{Name: "f"}}}
	r.Summarise(1)
	var buf bytes.Buffer
	if err := Text(&buf, r, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no findings") {
		t.Errorf("a clean scan must say so explicitly, not print an empty section:\n%s", buf.String())
	}
}

// Rule counts are sorted, so two runs of the same scan produce identical
// output. Go map iteration is randomised; without the sort a diff of two
// reports would show phantom changes.
func TestTextOutputIsDeterministic(t *testing.T) {
	r := sample()
	var first string
	for i := 0; i < 20; i++ {
		var buf bytes.Buffer
		if err := Text(&buf, r, 10); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = buf.String()
		} else if buf.String() != first {
			t.Fatalf("run %d differs from run 0 — map iteration order leaked into output", i)
		}
	}
}

// Long identifiers get truncated to keep the column aligned. Slicing bytes
// splits a multi-byte rune and emits mojibake; at 24 of 37 widths a CJK
// identifier produced invalid UTF-8.
func TestTruncateNeverSplitsARune(t *testing.T) {
	for _, s := range []string{
		"处理订单请求的函数名称很长需要被截断处理",
		"validateЛогинAndAuthoriseTheUserRequestWithRetries",
		"MüllerKäseÜberprüfungsfunktionZählerHandler",
		"emoji🔍in🔥an🚀identifier🎯which🌟is🍕long",
	} {
		for n := 2; n <= 40; n++ {
			got := truncate(s, n)
			if !utf8.ValidString(got) {
				t.Errorf("truncate(%q, %d) = %q: invalid UTF-8", s, n, got)
			}
			if c := utf8.RuneCountInString(got); c > n {
				t.Errorf("truncate(%q, %d) returned %d runes", s, n, c)
			}
		}
	}
}

func TestTruncateLeavesShortStringsAlone(t *testing.T) {
	for _, s := range []string{"", "f", "Server.Handle", "Ünïcödé"} {
		if got := truncate(s, 34); got != s {
			t.Errorf("truncate(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestSARIFIsValidAndComplete(t *testing.T) {
	var buf bytes.Buffer
	docs := map[string]string{"panic-in-library": "panic removes the caller's choice"}
	if err := SARIF(&buf, sample(), docs); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("SARIF output is not valid JSON: %v", err)
	}
	if doc["version"] != "2.1.0" {
		t.Errorf("version = %v, want 2.1.0 — GitHub rejects anything else", doc["version"])
	}
	run := doc["runs"].([]any)[0].(map[string]any)
	results := run["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	// The suggestion must reach the engineer: SARIF renders as a PR comment and
	// "panic in library code" without "return an error" is not actionable.
	first := results[0].(map[string]any)
	msg := first["message"].(map[string]any)["text"].(string)
	if !strings.Contains(msg, "return an error") {
		t.Errorf("suggestion dropped from the SARIF message: %q", msg)
	}

	loc := first["locations"].([]any)[0].(map[string]any)["physicalLocation"].(map[string]any)
	if loc["artifactLocation"].(map[string]any)["uri"] != "b.go" {
		t.Errorf("wrong file in location: %v", loc)
	}
	if loc["region"].(map[string]any)["startLine"].(float64) != 22 {
		t.Errorf("wrong line in location: %v", loc["region"])
	}
}

func TestSARIFDeduplicatesRulesAndSortsThem(t *testing.T) {
	r := &model.Report{Findings: []model.Finding{
		{Rule: "zebra", File: "a.go"}, {Rule: "alpha", File: "a.go"},
		{Rule: "zebra", File: "b.go"}, {Rule: "alpha", File: "b.go"},
	}}
	var buf bytes.Buffer
	if err := SARIF(&buf, r, nil); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	rules := doc["runs"].([]any)[0].(map[string]any)["tool"].(map[string]any)["driver"].(map[string]any)["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("got %d rule descriptors, want 2 deduplicated", len(rules))
	}
	if rules[0].(map[string]any)["id"] != "alpha" {
		t.Errorf("rules not sorted: %v — unsorted output makes every diff noisy", rules)
	}
}

func TestSARIFSeverityMapping(t *testing.T) {
	want := map[model.Severity]string{
		model.Error: "error", model.Warn: "warning", model.Info: "note",
	}
	for sev, level := range want {
		if got := sarifLevel(sev); got != level {
			t.Errorf("sarifLevel(%q) = %q, want %q", sev, got, level)
		}
	}
	if got := sarifLevel("something-new"); got != "note" {
		t.Errorf("unknown severity = %q, want it to degrade to note rather than break the upload", got)
	}
}

// An empty run must still be valid SARIF. GitHub rejects a malformed upload,
// and "the scan was clean" is exactly when you least want a failed step.
func TestSARIFOnACleanScan(t *testing.T) {
	var buf bytes.Buffer
	if err := SARIF(&buf, &model.Report{}, nil); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("clean scan produced invalid SARIF: %v", err)
	}
	if doc["version"] != "2.1.0" {
		t.Error("clean scan lost the version field")
	}
}

func TestJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	var back model.Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("JSON output does not parse back: %v", err)
	}
	if len(back.Funcs) != 2 || len(back.Findings) != 2 {
		t.Errorf("round trip lost records: %d funcs, %d findings", len(back.Funcs), len(back.Findings))
	}
	if back.Summary.FindingsTotal != 2 {
		t.Errorf("summary lost: %+v", back.Summary)
	}
}

// A closed pipe — `ratchet scan | head -5` — must surface as an error from the
// writers that report one, not be swallowed into a silently truncated file.
func TestWriteErrorsPropagate(t *testing.T) {
	for name, fn := range map[string]func(*failWriter) error{
		"CSV":   func(w *failWriter) error { return CSV(w, sample()) },
		"JSON":  func(w *failWriter) error { return JSON(w, sample()) },
		"SARIF": func(w *failWriter) error { return SARIF(w, sample(), nil) },
	} {
		if err := fn(&failWriter{}); err == nil {
			t.Errorf("%s swallowed a write error", name)
		}
	}
}

type failWriter struct{}

func (*failWriter) Write([]byte) (int, error) { return 0, errBroken }

var errBroken = errors.New("broken pipe")

func TestFindingsGroupsByFile(t *testing.T) {
	var buf bytes.Buffer
	Findings(&buf, []model.Finding{
		{File: "a.go", Line: 1, Col: 2, Rule: "r1", Message: "m1", Suggest: "do this"},
		{File: "a.go", Line: 5, Col: 1, Rule: "r2", Message: "m2"},
		{File: "b.go", Line: 9, Col: 3, Rule: "r1", Message: "m3"},
	})
	out := buf.String()
	if n := strings.Count(out, "a.go"); n != 1 {
		t.Errorf("a.go printed as a header %d times, want 1 — findings are not grouped", n)
	}
	if !strings.Contains(out, "do this") {
		t.Errorf("suggestion not shown:\n%s", out)
	}
	if !strings.Contains(out, "b.go") {
		t.Errorf("second file missing:\n%s", out)
	}
}

func TestFindingsOnAnEmptySlice(t *testing.T) {
	var buf bytes.Buffer
	Findings(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("no findings should print nothing, got %q", buf.String())
	}
}

// An imported finding about a whole file, such as an unused one, has no
// position; "0:0" would send someone to look for a line.
func TestFindingsWithoutALineOmitThePosition(t *testing.T) {
	var buf bytes.Buffer
	Findings(&buf, []model.Finding{
		{File: "lib/old.dart", Rule: "dcm:unused-files", Message: "Unused file"},
		{File: "lib/old.dart", Rule: "dcm:avoid-dynamic", Message: "Avoid dynamic.", Line: 3, Col: 5},
	})
	out := buf.String()
	if strings.Contains(out, "0:0") {
		t.Errorf("a finding without a line printed a bogus position:\n%s", out)
	}
	if !strings.Contains(out, "  dcm:unused-files") || !strings.Contains(out, "  3:5  dcm:avoid-dynamic") {
		t.Errorf("unexpected output:\n%s", out)
	}
}
