package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sherzing/assay/cmd/internal/cmdtest"
)

func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }

// The fixture is a tiny repository with a known set of violations and one
// annotated judgement, so a scan of it produces findings, measures and a
// verdict — all three record kinds strata has to store.
const fixtureSrc = `package core

import "errors"

// Decode is exported and takes any.
func Decode(v any) string {
	return v.(string)
}

// Load discards the error it just checked.
func Load(name string) error {
	err := open(name)
	if err != nil {
		return nil
	}
	return nil
}

// quality:accepted the exported shape is frozen until v2
func Widen(v any) string {
	s, _ := v.(string)
	return s
}

func open(name string) error { return errors.New("cannot open " + name) }
`

func fixtureRepo(t *testing.T) string {
	t.Helper()
	return cmdtest.Tree(t, map[string]string{"core/parse.go": fixtureSrc})
}

// ---------- the pipe ----------

// THE COMPOSITION TEST. The claim these four binaries make is that JSONL is the
// interface: no shared library, no plugin API, just a stream. That claim is
// only true if the real output of one binary loads into the next, so this runs
// both for real rather than calling store.Append with a handcrafted record.
func TestRatchetMeasuresLoadIntoStrata(t *testing.T) {
	ratchet := cmdtest.Build(t, "ratchet")
	strata := cmdtest.Build(t, "strata")
	repo := fixtureRepo(t)
	storeDir := filepath.Join(t.TempDir(), "store")

	emitted := ratchet.Run(t, repo, "scan", ".", "--emit", "measures",
		"--repo", "fixture", "--org", "-").MustPass(t)
	want := len(cmdtest.Lines(emitted.Stdout))
	if want == 0 {
		t.Fatal("ratchet emitted no measures, so the pipe carries nothing")
	}

	strata.Pipe(t, repo, emitted.Stdout, "append", "--store", storeDir).MustPass(t).
		MustSay(t, "appended "+strconv.Itoa(want)+" measures")

	strata.Run(t, repo, "stat", "--store", storeDir).MustPass(t).
		MustSay(t, "measures: "+strconv.Itoa(want), "repos:    fixture", "cognitive.p90")

	// A range query gets the records back out in the same shape they went in.
	q := strata.Run(t, repo, "query", "--store", storeDir, "--metric", "cognitive.p90").MustPass(t)
	lines := cmdtest.Lines(q.Stdout)
	if len(lines) != 1 {
		t.Fatalf("query returned %d lines for one project-scope metric:\n%s", len(lines), q)
	}
	var m struct {
		Kind   string  `json:"kind"`
		Repo   string  `json:"repo"`
		Metric string  `json:"metric"`
		Value  float64 `json:"value"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("query emitted unparseable JSONL: %v\n%s", err, lines[0])
	}
	if m.Kind != "measure" || m.Repo != "fixture" || m.Metric != "cognitive.p90" {
		t.Errorf("round trip lost information: %+v", m)
	}

	// A filter that matches nothing returns nothing, rather than everything.
	if got := strata.Run(t, repo, "query", "--store", storeDir,
		"--repo", "not-a-repo").MustPass(t).Stdout; strings.TrimSpace(got) != "" {
		t.Errorf("filtering on an unknown repo returned data:\n%s", got)
	}

	strata.Run(t, repo, "rollup", "--store", storeDir, "--period", "week").MustPass(t).
		MustSay(t, "fixture")
	strata.Run(t, repo, "query", "--store", storeDir, "--format", "csv").MustPass(t).
		MustSay(t, "ts,repo,commit,scope,path,metric,value")
	strata.Run(t, repo, "query", "--store", storeDir, "--since", "yesterday").MustFail(t).
		MustSay(t, "--since")
}

// Findings and verdicts travel on the same stream as measures, and a consumer
// must be able to pick out the kind it cares about.
func TestRatchetFindingsAndVerdictsLoadIntoStrata(t *testing.T) {
	ratchet := cmdtest.Build(t, "ratchet")
	strata := cmdtest.Build(t, "strata")
	repo := fixtureRepo(t)
	storeDir := filepath.Join(t.TempDir(), "store")

	emitted := ratchet.Run(t, repo, "scan", ".", "--emit", "findings",
		"--repo", "fixture", "--org", "acme").MustPass(t)

	findings, verdicts := 0, 0
	for _, l := range cmdtest.Lines(emitted.Stdout) {
		var rec struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("emitted line is not JSON: %v\n%s", err, l)
		}
		switch rec.Kind {
		case "finding":
			findings++
		case "verdict":
			verdicts++
		default:
			t.Errorf("unexpected record kind %q on the findings stream", rec.Kind)
		}
	}
	if findings == 0 || verdicts == 0 {
		t.Fatalf("expected both findings and verdicts on the stream, got %d and %d",
			findings, verdicts)
	}

	strata.Pipe(t, repo, emitted.Stdout, "append", "--store", storeDir).MustPass(t).
		MustSay(t, "appended 0 measures, "+strconv.Itoa(findings)+" findings, "+strconv.Itoa(verdicts)+" verdicts")

	v := strata.Run(t, repo, "verdicts", "--store", storeDir).MustPass(t)
	if got := len(cmdtest.Lines(v.Stdout)); got != verdicts {
		t.Errorf("verdicts resolved to %d records, want %d", got, verdicts)
	}
	v.MustSay(t, "accepted", "frozen until v2")

	// The rule filter is how a maintainer looks at one rule's evidence.
	strata.Run(t, repo, "verdicts", "--store", storeDir,
		"--rule", "no-such-rule").MustPass(t).MustNotSay(t, "frozen until v2")

	strata.Run(t, repo, "precision", "--store", storeDir).MustPass(t).
		MustSay(t, "any-in-exported-signature")
}

// Producers should not have to know where their output ends up, so append can
// stamp the repo name on records that carry none.
func TestAppendStampsTheRepoOnUnnamedRecords(t *testing.T) {
	strata := cmdtest.Build(t, "strata")
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")

	in := `{"v":1,"kind":"measure","ts":"2026-09-01T00:00:00Z","scope":"project","metric":"cognitive.p90","value":12}
{"v":1,"kind":"measure","repo":"already-named","ts":"2026-09-01T00:00:00Z","scope":"project","metric":"cognitive.p90","value":9}
`
	strata.Pipe(t, dir, in, "append", "--store", storeDir, "--repo", "stamped").MustPass(t).
		MustSay(t, "appended 2 measures")

	out := strata.Run(t, dir, "query", "--store", storeDir).MustPass(t).Stdout
	if !strings.Contains(out, `"repo":"stamped"`) {
		t.Errorf("the unnamed record was not stamped:\n%s", out)
	}
	if !strings.Contains(out, `"repo":"already-named"`) {
		t.Errorf("--repo overwrote a name the producer had already set:\n%s", out)
	}
}

// A malformed line must not kill a long pipe. One bad producer in a chain
// should cost you that record, not the run.
func TestAppendSkipsGarbageWithoutDying(t *testing.T) {
	strata := cmdtest.Build(t, "strata")
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")

	in := `{"v":1,"kind":"measure","repo":"r","ts":"2026-09-01T00:00:00Z","scope":"project","metric":"m","value":1}
not json at all
# a comment

{"v":1,"kind":"unheard-of","repo":"r"}
{"v":1,"kind":"measure","repo":"r","ts":"2026-09-01T00:00:00Z","scope":"project","metric":"m","value":2}
`
	strata.Pipe(t, dir, in, "append", "--store", storeDir).MustPass(t).
		MustSay(t, "appended 2 measures")
}

func TestStatOnAnEmptyStore(t *testing.T) {
	strata := cmdtest.Build(t, "strata")
	dir := t.TempDir()
	strata.Run(t, dir, "stat", "--store", filepath.Join(dir, "store")).MustPass(t).
		MustSay(t, "measures: 0", "verdicts: 0")
	strata.Run(t, dir, "precision", "--store", filepath.Join(dir, "store")).MustPass(t).
		MustSay(t, "no rules with enough judged findings yet")
}

// ---------- verify ----------

func goodEvidence() string {
	return `{"schema":"assay-evidence/1","rule":"no-context-todo","org":"acme","period":"2026-Q3",` +
		`"repos":4,"fixed":31,"carried":6,"falsePositive":3,"judged":40,"precision":0.93}`
}

// THE ALLOWLIST, END TO END. The whole point of the evidence format is that an
// organisation can share it without sharing anything about its code, and the
// defence is an allowlist: an unknown field is refused rather than waved past.
// Unit tests cover the verifier; this asserts the CLI actually fails the build
// on arrival, which is where the guarantee is worth something.
func TestVerifyRejectsAnUnknownField(t *testing.T) {
	strata := cmdtest.Build(t, "strata")
	dir := t.TempDir()

	leaks := map[string]string{
		"file":         `"internal/impl/pool.go"`,
		"fingerprints": `["9f2a3c4d5e6f7081"]`,
		"repoNames":    `["billing-api"]`,
		"reason":       `"needs the v2 migration"`,
		"by":           `"someone@acme.example"`,
		"anythingNew":  `"whatever a future exporter decides to add"`,
	}
	for field, val := range leaks {
		t.Run(field, func(t *testing.T) {
			leaky := strings.TrimSuffix(goodEvidence(), "}") +
				`,"` + field + `":` + val + `}`
			cmdtest.WriteFile(t, dir, field+".jsonl", goodEvidence()+"\n"+leaky+"\n")

			r := strata.Run(t, dir, "verify", field+".jsonl").MustFail(t)
			// Naming the field and the line is the difference between a
			// rejection someone can act on and one they will work around.
			r.MustSay(t, field, ":2", "REJECT", "blocking")
			r.MustNotSay(t, "safe to share")
		})
	}
}

func TestVerifyAcceptsACleanFile(t *testing.T) {
	strata := cmdtest.Build(t, "strata")
	dir := t.TempDir()
	cmdtest.WriteFile(t, dir, "clean.jsonl",
		goodEvidence()+"\n# a comment line\n\n"+goodEvidence()+"\n")
	strata.Run(t, dir, "verify", "clean.jsonl").MustPass(t).
		MustSay(t, "2 records, safe to share")
	strata.Run(t, dir, "verify").MustFail(t).MustSay(t, "usage: strata verify")
}

// SUSPECT: strata does not accept flags after a positional argument, and the
// error blames the wrong thing. `strata verify f.jsonl --quiet` reports
// "open --quiet: no such file or directory" rather than honouring the flag or
// saying the flag is misplaced. ratchet solved exactly this with parseArgs (see
// cmd/ratchet/main.go); strata's subcommands still call fs.Parse directly.
// Asserting current behaviour; not fixed here.
func TestStrataIgnoresFlagsAfterAPositional(t *testing.T) {
	strata := cmdtest.Build(t, "strata")
	dir := t.TempDir()
	cmdtest.WriteFile(t, dir, "clean.jsonl", goodEvidence()+"\n")

	strata.Run(t, dir, "verify", "--quiet", "clean.jsonl").MustPass(t).
		MustNotSay(t, "safe to share") // --quiet honoured before the positional

	// MustFail is the canary: if strata ever learns ratchet's trick, this test
	// starts failing and the SUSPECT note above should be deleted with it.
	strata.Run(t, dir, "verify", "clean.jsonl", "--quiet").MustFail(t).
		MustSay(t, "--quiet", "no such file")
}

// ---------- export ----------

// The export path has to satisfy its own verifier. An exporter and a verifier
// that disagree is precisely the bug that would leak something, so this runs
// the real export through the real verify.
func TestExportRoundTripsThroughVerify(t *testing.T) {
	ratchet := cmdtest.Build(t, "ratchet")
	strata := cmdtest.Build(t, "strata")
	repo := fixtureRepo(t)
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")

	emitted := ratchet.Run(t, repo, "scan", ".", "--emit", "findings",
		"--repo", "fixture", "--org", "acme").MustPass(t)
	strata.Pipe(t, repo, emitted.Stdout, "append", "--store", storeDir).MustPass(t)

	exported := strata.Run(t, dir, "export", "--store", storeDir,
		"--org", "acme", "--period", "2026-Q3", "--min-judged", "1").MustPass(t)
	if len(cmdtest.Lines(exported.Stdout)) == 0 {
		t.Fatalf("export produced no records:\n%s", exported)
	}
	// What leaves the network is arithmetic. If a path, a fingerprint or a repo
	// name ever appears here, the whole privacy story is gone.
	for _, forbidden := range []string{"core/parse.go", "fixture", "frozen until v2", "fingerprint"} {
		if strings.Contains(exported.Stdout, forbidden) {
			t.Errorf("export leaked %q:\n%s", forbidden, exported.Stdout)
		}
	}

	cmdtest.WriteFile(t, dir, "shared.jsonl", exported.Stdout)
	strata.Run(t, dir, "verify", "shared.jsonl").MustPass(t).MustSay(t, "safe to share")

	strata.Run(t, dir, "export", "--store", storeDir).MustFail(t).MustSay(t, "org")
}

// promote-check answers "has this rule earned its place", and from one
// organisation's store the honest answer is always no.
func TestPromoteCheckFromALocalStoreCannotMeetTheBar(t *testing.T) {
	ratchet := cmdtest.Build(t, "ratchet")
	strata := cmdtest.Build(t, "strata")
	repo := fixtureRepo(t)
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")

	emitted := ratchet.Run(t, repo, "scan", ".", "--emit", "findings",
		"--repo", "fixture", "--org", "acme").MustPass(t)
	strata.Pipe(t, repo, emitted.Stdout, "append", "--store", storeDir).MustPass(t)

	r := strata.Run(t, dir, "promote-check", "--store", storeDir).MustPass(t)
	r.MustSay(t, "the multi-org bar cannot be met from here", "not yet")
	strata.Run(t, dir, "promote-check", "--store", storeDir, "--strict").MustFail(t)
	strata.Run(t, dir, "promote-check", "--store", storeDir, "--json").MustPass(t).
		MustSay(t, `"Outcome"`)
}

func TestStrataUsageAndVersion(t *testing.T) {
	strata := cmdtest.Build(t, "strata")
	dir := t.TempDir()
	strata.Run(t, dir, "version").MustPass(t).MustSay(t, "strata "+version)
	if got := strata.Run(t, dir, "quarry").MustFail(t).Code; got != 2 {
		t.Errorf("exit %d for an unknown command, want 2", got)
	}
	if got := strata.Run(t, dir).MustFail(t).Code; got != 2 {
		t.Errorf("exit %d for no arguments, want 2", got)
	}
}
