package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sherzing/assay/cmd/internal/cmdtest"
)

func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }

// Several violations across two files, one of them judged a false positive, so
// a plan has something to group and something to refuse to file.
const fixtureSrc = `package core

import "errors"

// Decode asserts without comma-ok.
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

// quality:false-positive the wire format fixes this signature
func Passthrough(v any) any { return v }

func open(name string) error { return errors.New("cannot open " + name) }
`

const fixtureOther = `package plugin

// Register panics rather than returning an error.
func Register(name string) {
	panic("not implemented: " + name)
}

// Lookup asserts without comma-ok.
func Lookup(m map[string]any, k string) string {
	return m[k].(string)
}
`

// findings runs a real scan, because docket's input contract is ratchet's
// output contract and a handwritten fixture would let the two drift apart.
func findings(t *testing.T) string {
	t.Helper()
	ratchet := cmdtest.Build(t, "ratchet")
	repo := cmdtest.Tree(t, map[string]string{
		"core/parse.go":  fixtureSrc,
		"plugin/hook.go": fixtureOther,
	})
	return ratchet.Run(t, repo, "scan", ".", "--emit", "findings",
		"--repo", "fixture", "--org", "-").MustPass(t).Stdout
}

// A false positive means the RULE is wrong. Filing work for it would put the
// cost of a bad rule onto an engineer, which is the behaviour that teaches
// people to ignore the tool.
func TestPlanGroupsFindingsAndRefusesToFileFalsePositives(t *testing.T) {
	docket := cmdtest.Build(t, "docket")
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")

	r := docket.Pipe(t, dir, findings(t), "plan", "--store", storeDir).MustPass(t)
	r.MustSay(t, "marked false-positive", "fix the rule, do not file work",
		"tickets covering", "grouped by theme")
	r.MustNotSay(t, "wire format") // the reason is not a ticket title

	// Grouping is the whole design: one ticket per theme, not one per finding.
	byFinding := docket.Pipe(t, dir, findings(t), "plan",
		"--store", storeDir, "--group-by", "finding").MustPass(t)
	if countTickets(t, r.Stdout) >= countTickets(t, byFinding.Stdout) {
		t.Errorf("grouping by theme did not produce fewer tickets than one-per-finding\n"+
			"theme:\n%s\nfinding:\n%s", r.Stdout, byFinding.Stdout)
	}

	// --min-size is how a team says "do not file a ticket for two things".
	big := docket.Pipe(t, dir, findings(t), "plan",
		"--store", storeDir, "--min-size", "99").MustPass(t)
	big.MustSay(t, "nothing to ticket")

	docket.Pipe(t, dir, findings(t), "plan", "--store", storeDir, "-v").MustPass(t).
		MustSay(t, "───")
}

// THE SAFETY PROPERTY. docket writes into someone else's issue tracker, so a
// first run must never create anything. If --yes were the default, the first
// person to try the tool would spray a backlog into a shared workspace.
func TestCreateIsAPreviewUnlessYesIsGiven(t *testing.T) {
	docket := cmdtest.Build(t, "docket")
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")

	preview := docket.Pipe(t, dir, findings(t), "create",
		"--store", storeDir, "--provider", "file").MustPass(t)
	preview.MustSay(t, "PREVIEW — nothing will be created", "Add --yes")

	// Nothing recorded and nothing written, whatever the preview printed.
	docket.Run(t, dir, "status", "--store", storeDir).MustPass(t).
		MustSay(t, "no tickets recorded")
	if _, err := os.Stat(filepath.Join(storeDir, "tickets", "md")); !os.IsNotExist(err) {
		t.Errorf("a preview wrote ticket files to disk (stat err: %v)", err)
	}
}

// The full life cycle against the file provider — no network, no external
// tracker, but the same code path a real one takes.
func TestCreateStatusSyncAndClose(t *testing.T) {
	docket := cmdtest.Build(t, "docket")
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	in := findings(t)

	created := docket.Pipe(t, dir, in, "create",
		"--store", storeDir, "--provider", "file", "--yes").MustPass(t)
	created.MustSay(t, "created", "T-001", "recorded in")

	status := docket.Run(t, dir, "status", "--store", storeDir).MustPass(t)
	status.MustSay(t, "T-001", "open", "findings covered")

	// A second run must not re-file work that is already tracked. A tool that
	// duplicates its own tickets gets deleted.
	again := docket.Pipe(t, dir, in, "plan", "--store", storeDir).MustPass(t)
	again.MustSay(t, "already covered by a ticket")
	again.MustNotSay(t, "tickets covering")

	// The same findings still present: nothing is resolved.
	docket.Pipe(t, dir, in, "sync", "--store", storeDir, "--provider", "file").MustPass(t).
		MustSay(t, "T-001", "nothing fully resolved yet")

	// The findings stop reproducing. That is the finish line a cohort ticket
	// gets to have, and it only exists because `ratchet check` stops new ones
	// joining the set.
	empty := docket.Pipe(t, dir, "", "sync", "--store", storeDir, "--provider", "file").MustPass(t)
	empty.MustSay(t, "ready to close", "Add --yes to close them")

	closed := docket.Pipe(t, dir, "", "sync",
		"--store", storeDir, "--provider", "file", "--yes").MustPass(t)
	closed.MustSay(t, "closed T-001")

	docket.Run(t, dir, "status", "--store", storeDir).MustPass(t).
		MustSay(t, "closed")
	docket.Pipe(t, dir, "", "sync", "--store", storeDir, "--provider", "file").MustPass(t).
		MustSay(t, "no open tickets")
}

// Without input the tool says what to pipe in. Exiting 0 on an empty plan would
// make a broken pipeline look like a clean codebase.
func TestPlanWithoutInputSaysWhatToDo(t *testing.T) {
	docket := cmdtest.Build(t, "docket")
	dir := t.TempDir()
	docket.Run(t, dir, "plan", "--store", filepath.Join(dir, "store")).MustFail(t).
		MustSay(t, "no input", "ratchet scan . --emit findings")
}

func TestUnknownProviderIsAnError(t *testing.T) {
	docket := cmdtest.Build(t, "docket")
	dir := t.TempDir()
	docket.Pipe(t, dir, findings(t), "create", "--store", filepath.Join(dir, "store"),
		"--provider", "carrier-pigeon", "--yes").MustFail(t).
		MustSay(t, "unknown provider")
}

func TestDocketUsageAndVersion(t *testing.T) {
	docket := cmdtest.Build(t, "docket")
	dir := t.TempDir()
	docket.Run(t, dir, "version").MustPass(t).MustSay(t, "docket "+version)
	docket.Run(t, dir, "help").MustPass(t).MustSay(t, "keep them honest")
	if got := docket.Run(t, dir, "ledger").MustFail(t).Code; got != 2 {
		t.Errorf("exit %d for an unknown command, want 2", got)
	}
	if got := docket.Run(t, dir).MustFail(t).Code; got != 2 {
		t.Errorf("exit %d for no arguments, want 2", got)
	}
}

// countTickets reads the "N tickets covering M findings" line.
func countTickets(t *testing.T, out string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "tickets covering") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, err := strconv.Atoi(f); err == nil {
				return v
			}
		}
	}
	t.Fatalf("no ticket count in output:\n%s", out)
	return 0
}
