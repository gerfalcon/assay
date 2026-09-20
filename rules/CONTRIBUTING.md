# Contributing a rule

Rules here are admitted on **evidence**, not on argument. A rule that cannot show
it is mostly right on real code does not ship, however sensible it sounds.

That is the whole point of this repository. Anyone can write a rule; the scarce
thing is knowing which rules are worth running, and that only comes from many
codebases and many independent judgements. No vendor can assemble that. A
community can.

---

## The three tiers

```
rules/core/     shipped with assay. Has published precision data.
rules/org/      an organisation's own pack. Private or public.
rules/local/    project-specific, lives in the repo being analysed.
```

Resolution is **local → org → core**, most specific wins. A rule starts local,
earns its way up.

## Promotion thresholds

To move from `org/` to `core/`:

| criterion | why |
|---|---|
| judged findings from **≥ 2 organisations** | not one team's house style |
| **≥ 50** judged findings total | enough to mean something |
| **precision ≥ 0.80** | it is mostly right |
| **≥ 3 distinct repositories** | not tuned to one codebase |

Check your standing:

```sh
strata precision --rule my-rule                 # your own evidence
strata promote-check --evidence evidence/       # against the whole corpus
```

```
no-context-todo                PROMOTE
                                 2 orgs · 63 judged · 4 repos · precision 0.92

new-idea                       not yet
                                 ✗ needs 1 more organisations (has 1, wants 2)
                                 ✗ needs 37 more judged findings (has 13, wants 50)
                                 ✗ needs 2 more repositories (has 1, wants 3)
```

It names the gap rather than just failing, so you know what is still missing.

**Rules can lose.** A rule with enough evidence to conclude and precision below
the bar gets `REJECT`, not `not yet` — 73 judged findings at 25% precision is a
conclusion, not a shortage of data. Withdraw it, or fix it and start the evidence
again. Without that outcome a corpus only ever grows and its average quality only
ever falls, which is how every rule catalogue ends up ignored.

Precision is **recomputed from summed counts**, never averaged across
contributors. Averaging would let a small contributor with a lucky 1.00 drag up a
rule that is mostly wrong everywhere else.

**Carriage is not a failure.** A rule where most confirmed findings are *carried*
rather than fixed is still a good rule — it is pointing at something real that
the ecosystem has decided to live with. A known framework defect everyone works
around belongs in core as `severity: INFO`, because knowing about it is useful
even when fixing it is not an option. Say so in the metadata.

---

## Attributing evidence to an organisation

The ≥2-organisations bar is what stops a rule being promoted on one team's house
style, so the attribution has to be right. Three sources, most reliable first:

```sh
ratchet scan . --org acme              # explicit, wins over everything
```
```yaml
# .quality.yaml — set once per repo instead of on every invocation
org: acme
```
```
# last resort: inferred from the committer's email domain
```

**The inference declines to guess.** A consumer mail provider — gmail, outlook,
proton, iCloud, a GitHub noreply address — yields *nothing* rather than an
organisation called "gmail". No attribution is an honest answer; a wrong one
quietly inflates the count that gates promotion.

When an org is inferred rather than stated, `ratchet` says so on stderr. Evidence
gets tagged with it, and discovering after publication that your work was
attributed to whatever sat in your global git config is a surprise nobody wants.
`--org=-` opts out of attribution entirely.

## Submitting precision data without submitting code

**This is the part that makes the model work for closed-source teams.** You can
contribute the evidence for a rule without sharing a line of source, a file path,
or a fingerprint.

```sh
strata precision --json > precision.json
```

Strip it to the aggregate — `scripts/export-precision.sh` does this — and what
leaves your network is:

```json
{
  "rule": "no-context-todo",
  "org": "acme",
  "repos": 4,
  "fixed": 31,
  "carried": 6,
  "falsePositive": 2,
  "precision": 0.95,
  "period": "2026-Q3"
}
```

No paths. No code. No fingerprints. No repository names beyond a count. Open a PR
adding that file under `evidence/<rule-id>/<org>-<period>.json`.

If even a rule name is sensitive, say so in the PR and we will work out what can
be shared. Partial evidence beats none.

---

## Rule format

Rules are **semgrep** YAML. We do not invent a rule language — semgrep already
has one, it is OSS, it runs locally, and other tools already read it.

```yaml
rules:
  - id: kebab-case-and-descriptive
    languages: [go]
    severity: ERROR | WARNING | INFO
    message: >-
      What is wrong, and what to do instead. Written for the engineer who will
      read it at 4pm on a Friday, not for a rule catalogue.
    metadata:
      category: correctness | reliability | maintainability | security
      evidence: how you established this is mostly right
      confidence: high | medium | low
      note: known false-positive classes, if any
    pattern: ...
    paths:
      exclude: ["*_test.go"]
```

### What a rule must have

- **An actionable message.** "Avoid X" is not actionable. "X breaks when Y; use Z"
  is. If you cannot say what to do instead, the rule is not ready.
- **Evidence in the metadata.** How do you know this is mostly right?
- **Honest confidence.** `low` is a perfectly good value and it lets people
  choose.
- **Known false positives named.** Every rule has some. Hiding them wastes other
  people's triage.
- **A good/bad corpus.** See below. A rule with no negative cases has not been
  thought about hard enough.

### The good/bad corpus

Every built-in rule carries a pair of files, and a test asserts that no rule
exists without them:

```
internal/analyze/testdata/rules/<rule-id>/
  bad.go    every line ending `// want` must produce a finding,
            and no other line may produce one
  good.go   must produce no unjudged finding at all
```

**The good file is the more valuable half.** It is where you record the shapes
that look like violations and are not — and those are not hypothetical. Three of
the five original rules were noise on first contact with production code, all
for reasons now pinned in `good.go`:

| shape | why it is not a defect |
|---|---|
| `func Exec(..., args ...any)` | variadic pass-through is the idiom behind `fmt.Printf` and every SQL driver |
| `panic` inside `mustParse` | the `must*` prefix is Go's own announcement of a deliberate panic |
| `//nolint:forcetypeassert` | an existing suppression another tool already honours |
| `return nil` in a `WalkDir` callback | that is the API's skip signal, not a swallowed error |

A rule that gets greedier now fails in CI instead of on someone's repository.
When you find a new false-positive class in the wild, the fix is a case in
`good.go` — that is what stops it coming back.

Marking a line `// want-unjudged` in `good.go` asserts it *should* still produce
an unjudged finding; it is used to pin the annotation scope and you will rarely
need it.

### What gets a rule rejected

- **No evidence.** The most common rejection, and not negotiable.
- **It is a style preference.** If two reasonable teams would disagree, it is an
  org rule.
- **It duplicates an existing linter.** If `golangci-lint` already catches it,
  configure that instead.
- **Untestable message.** If the fix depends on context the rule cannot see, it
  will produce noise.

---

## How to establish evidence

The method used for the existing core rules, which you are welcome to copy:

1. **Probe, do not assume.** Count the pattern per kLOC across codebases you have
   independent reason to consider healthy.
2. **Near-zero everywhere → candidate.** Those teams hold the convention.
3. **Common everywhere → reject it.** They do not hold it, whatever the style
   guides say. `naked return` failed exactly this test at 3.6 and 4.4 per kLOC.
4. **Divergent between them → org rule, not core.** `interface{}` vs `any`
   differed by 300× between two good codebases, and the reason was that one
   predates the `any` alias. Age wearing the costume of quality.
5. **Then run it and read every finding.** Three of assay's own five original
   rules were noise on first contact with production code. Statistics narrow the
   candidates; only reading the output confirms them.

## Review

A rule PR needs: the rule, evidence for it, and one maintainer who has run it
against a codebase they did not write. That last requirement exists because it is
the only check that reliably catches a rule that is technically correct and
practically useless.

Maintainers work from [MAINTAINING.md](MAINTAINING.md), which also documents how
rules get **demoted**. A rule that was right in 2026 and wrong in 2028 is not a
failure — it did its job and the ecosystem changed. Rules losing their place is
what stops the corpus becoming another catalogue nobody reads.

## Finding candidates in the first place

If you have codebases you already trust, let them tell you what to propose:

```sh
ratchet learn repo-a repo-b --emit
```

```
no-context-todo             0.07    0.00   held — ship it
prefer-any-over-interface  21.13    0.07   divergent — org rule
no-naked-return             3.77    4.48   NOT held — reject
```

**The rejections are the valuable part.** `no-naked-return` is on every Go style
list and both codebases use it several times per thousand lines — shipping it
would have produced thousands of findings nobody agrees are defects.

`--emit` prints draft rules with the evidence already filled in. They are drafts:
the method finds *conventions*, not defects, and a codebase can consistently do
something bad.
