// Command plumb checks a codebase against the architecture declared in
// ARCHITECTURE.md.
//
// A plumb line is the oldest conformance tool there is: you declare vertical,
// and the string tells you the truth. It does not have an opinion about where
// the wall should go.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sherzing/assay/internal/arch"
	"github.com/sherzing/assay/internal/baseline"
	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/internal/report"
	"github.com/sherzing/assay/pkg/schema"
)

const usage = `plumb — check a codebase against the architecture it declares

  plumb check [dir]      fail on violations not in the baseline
  plumb scan  [dir]      report every violation, exit 0
  plumb baseline [dir]   record today's violations as tolerated
  plumb diff  [dir]      classify a declaration change as tightening or loosening

Flags
  --doc <path>       declaration (default ARCHITECTURE.md)
  --file <path>      baseline file (default .plumb-baseline.json)
  --emit findings    write JSONL to stdout for strata/docket
  --include-tests    follow imports from _test.go files
  --base <ref>       git ref to diff the declaration against (default origin/main)
  --force            overwrite an existing baseline
  --tighten          drop baseline entries that no longer reproduce

The declaration is a fenced ` + "```arch" + ` block inside the document:

    layer domain   internal/domain
    layer infra    internal/impl internal/store
    forbid domain -> infra

Checks are TRANSITIVE. A direct-import rule misses domain -> helper -> infra,
which is the same dependency one hop away and is what an ordinary refactor
produces.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "check":
		err = run(args, true)
	case "scan":
		err = run(args, false)
	case "baseline":
		err = cmdBaseline(args)
	case "diff":
		err = cmdDiff(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "plumb: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "plumb:", err)
		os.Exit(1)
	}
}

type opts struct {
	dir, doc, file, emit, base   string
	includeTests, force, tighten bool
}

// parseArgs accepts flags on either side of the positional argument. Go's flag
// package stops at the first non-flag, which silently drops every flag placed
// after the path — a failure that looks like the flag having no effect.
func parseArgs(args []string) (opts, error) {
	o := opts{doc: "ARCHITECTURE.md", file: ".plumb-baseline.json", base: "origin/main"}
	fs := flag.NewFlagSet("plumb", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.doc, "doc", o.doc, "architecture declaration")
	fs.StringVar(&o.file, "file", o.file, "baseline path")
	fs.StringVar(&o.emit, "emit", "", "emit JSONL: findings")
	fs.StringVar(&o.base, "base", o.base, "git ref to diff against")
	fs.BoolVar(&o.includeTests, "include-tests", false, "follow test imports")
	fs.BoolVar(&o.force, "force", false, "overwrite an existing baseline")
	fs.BoolVar(&o.tighten, "tighten", false, "drop entries that no longer reproduce")

	var positional []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return o, err
		}
		rest = fs.Args()
		if len(rest) > 0 {
			positional = append(positional, rest[0])
			rest = rest[1:]
		}
	}
	if len(positional) > 1 {
		return o, fmt.Errorf("expected one directory, got %d: %s", len(positional), strings.Join(positional, " "))
	}
	o.dir = "."
	if len(positional) == 1 {
		o.dir = positional[0]
	}
	return o, nil
}

// load reads the declaration and builds the report.
func load(o opts) (*arch.Decl, *arch.Graph, []arch.Violation, *model.Report, error) {
	docPath := o.doc
	if !filepath.IsAbs(docPath) {
		docPath = filepath.Join(o.dir, o.doc)
	}
	raw, err := os.ReadFile(docPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("reading %s: %w\n\nWrite one first — see the preparation checklist in the README. "+
			"A tool that passes because there is nothing to check is worse than no tool", docPath, err)
	}
	d, err := arch.ParseDoc(string(raw))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("%s: %w", o.doc, err)
	}
	d.SHA = declSHA(o.dir, o.doc)

	g, err := arch.LoadGoGraph(o.dir, o.includeTests)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	vs := arch.Check(d, g)
	return d, g, vs, arch.Report(d, g, vs), nil
}

func run(args []string, gate bool) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	d, g, vs, rep, err := load(o)
	if err != nil {
		return err
	}

	if o.emit == "findings" {
		enc := schema.NewEncoder(os.Stdout)
		for i := range rep.Findings {
			f := rep.Findings[i]
			if err := enc.Write(&schema.Finding{
				Tool: "plumb", Rule: f.Rule, Severity: schema.SevError,
				File: f.File, Message: f.Message, Suggest: f.Suggest,
				Symbol: d.SHA, Fingerprint: f.Fingerprint,
			}); err != nil {
				return err
			}
		}
		return enc.Flush()
	}

	for _, dead := range arch.DeadLayers(d, g) {
		fmt.Fprintf(os.Stderr, "warning: layer %q matches no package — a rule guarding a directory that does not exist protects nobody\n", dead)
	}

	if !gate {
		printViolations(vs, len(g.Edges), len(d.Forbids))
		return nil
	}

	b, err := baseline.Load(filepath.Join(o.dir, o.file))
	if err != nil {
		return fmt.Errorf("no baseline at %s: run `plumb baseline` first.\n"+
			"Adopting on an existing codebase means recording today's violations and failing only on new ones —\n"+
			"starting from zero is how a rule gets switched off in the first afternoon", o.file)
	}
	res := b.Check(rep, false)
	fmt.Printf("%d tolerated, %d new, %d fixed\n", res.Existing, len(res.New), len(res.Fixed))
	if len(res.Fixed) > 0 {
		fmt.Printf("\n%d no longer present — `plumb baseline --tighten` locks that in\n", len(res.Fixed))
	}
	if len(res.New) > 0 {
		fmt.Printf("\nNEW violations (these fail the build):\n\n")
		report.Findings(os.Stdout, res.New)
		return fmt.Errorf("%d new architecture violations", len(res.New))
	}
	return nil
}

func printViolations(vs []arch.Violation, pkgs, rules int) {
	fmt.Printf("%d packages, %d rules, %d violations\n", pkgs, rules, len(vs))
	if len(vs) == 0 {
		return
	}
	cur := ""
	for _, v := range vs {
		if v.Forbid.String() != cur {
			cur = v.Forbid.String()
			fmt.Printf("\nforbid %s\n", cur)
		}
		fmt.Printf("  %s\n", strings.Join(v.Chain, " → "))
		fmt.Printf("      %s\n", v.Suggest())
	}
}

func cmdBaseline(args []string) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	_, _, _, rep, err := load(o)
	if err != nil {
		return err
	}
	path := filepath.Join(o.dir, o.file)

	if existing, err := baseline.Load(path); err == nil {
		if o.tighten {
			n := existing.Tighten(rep)
			if err := existing.Save(path); err != nil {
				return err
			}
			fmt.Printf("dropped %d violations that no longer reproduce; %d remain tolerated\n", n, len(existing.Tolerated))
			return nil
		}
		if !o.force {
			// The guard that makes the ratchet mean anything: without it,
			// "fix the failure" becomes "rewrite the baseline".
			return fmt.Errorf("baseline already exists at %s\n"+
				"  regenerating it would silently forgive every current violation.\n"+
				"  use --tighten to drop only what is genuinely fixed, or --force to overwrite", o.file)
		}
	}
	b := baseline.From(rep, gitSHA(o.dir))
	if err := b.Save(path); err != nil {
		return err
	}
	fmt.Printf("%d tolerated violations\n\ncommit this file — it is the record of what we agreed to tolerate\n", len(b.Tolerated))
	return nil
}

func cmdDiff(args []string) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	cur, err := os.ReadFile(filepath.Join(o.dir, o.doc))
	if err != nil {
		return err
	}
	newD, err := arch.ParseDoc(string(cur))
	if err != nil {
		return fmt.Errorf("current %s: %w", o.doc, err)
	}

	out, err := gitShow(o.dir, o.base+":"+o.doc)
	if err != nil {
		fmt.Printf("architecture declaration: new (no %s at %s)\n", o.doc, o.base)
		return nil
	}
	oldD, err := arch.ParseDoc(out)
	if err != nil {
		fmt.Printf("architecture declaration: previous version did not parse (%v); treating as new\n", err)
		return nil
	}

	c, detail := arch.Diff(oldD, newD)
	fmt.Print(arch.FormatDiff(c, detail))
	if c.NeedsReview() {
		return fmt.Errorf("declaration weakened")
	}
	return nil
}

func declSHA(dir, doc string) string {
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%H", "--", doc).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitSHA(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitShow(dir, ref string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "show", ref).Output()
	return string(out), err
}
