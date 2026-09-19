# Maintaining the rule corpus

For maintainers reviewing rule and evidence submissions. Contributors want
[CONTRIBUTING.md](CONTRIBUTING.md).

The corpus is only worth anything if its average quality holds. That means most
of this job is **saying no**, and — harder — **removing things that used to be
right**.

---

## Reviewing a rule submission

CI has already checked the mechanical parts: schema, allowlist, count
consistency, no leaked paths or fingerprints. What is left is judgement.

### The one check that cannot be automated

> **Has a maintainer run this rule against a codebase they did not write?**

Nothing else reliably catches a rule that is technically correct and practically
useless. A rule can have clean evidence, a sensible message and a valid pattern,
and still fire forty times on a codebase where every hit is idiomatic. You find
that out by running it, and only by running it.

```sh
semgrep --config path/to/new-rule.yaml --sarif /some/real/repo | ratchet import - --mode report
```

Read **every** finding. Not a sample. If more than a couple are wrong, the rule
is not ready regardless of what its evidence says.

### Then ask

- **Is the message actionable?** "Avoid X" is not. "X breaks when Y; use Z" is.
  If the fix depends on context the rule cannot see, it will generate noise.
- **Does an existing linter already catch it?** If `golangci-lint` has it,
  configure that instead. A second tool reporting the same thing doubles triage
  for no gain.
- **Would two reasonable teams disagree?** Then it is an org rule. Core is for
  defects, not preferences.
- **Are the known false positives named?** Every rule has some. A submission
  claiming none has not been run hard enough.
- **Is the confidence honest?** `low` is a perfectly good value. A rule marked
  `high` on 50 findings from one quarter is overclaiming.

### Check the evidence, do not just trust it

```sh
strata promote-check --evidence evidence/ --rule the-new-rule
```

Watch for:

- **All evidence from one period.** A rule that held for a quarter is weaker than
  one that held for a year. `promote-check` flags this.
- **One org contributing almost everything.** Technically two orgs, effectively
  one. The bar is about independence, not arithmetic.
- **Suspiciously round precision.** 1.00 over 60 findings deserves a question. No
  real rule is never wrong.

---

## Rules must be able to lose

This is the part every rule catalogue gets wrong, and it is why most of them end
up ignored.

**A corpus that only grows has an average quality that only falls.** Every rule
added is one more thing a newcomer has to evaluate, and if nothing is ever
removed the ratio of signal to obligation gets worse every release.

### Demotion triggers

Re-run `promote-check` over the whole corpus each release:

```sh
strata promote-check --evidence evidence/ --strict
```

| signal | action |
|---|---|
| precision falls below 0.80 with ≥50 judged | **demote to `org/`** and say why in the changelog |
| `REJECT` outcome | remove from core |
| carriage climbs above 0.80 | **change severity to INFO** — the rule is right, people are not fixing it |
| no new evidence for four quarters | mark stale; the world may have moved on |

### Demoting is not a failure

A rule that was right in 2026 and wrong in 2028 is a rule that did its job and
then the ecosystem changed. Say so plainly in the changelog. Quietly leaving it
in place because removing it feels like an admission is how catalogues rot.

The one thing not to do: **defend a rule against its own evidence.** If the data
says it is mostly wrong, it is mostly wrong. The whole premise of this repository
is that evidence beats argument, and that has to hold when the evidence is
inconvenient.

---

## Reviewing evidence submissions

Mechanically verified by CI. Two things to look at by eye:

**Does the shape make sense?** A rule with 400 judged findings from one org in
one quarter suggests either a very large codebase or a triage session that
rubber-stamped a batch. Ask.

**Is it plausibly independent?** Two subsidiaries of the same company are not two
organisations for the purpose of this bar. Nobody will enforce this but you.

---

## Adding to the probe catalogue

`ratchet learn` ships a catalogue of candidate patterns. When adding to it:

**Include patterns you expect to fail.** The catalogue deliberately carries
`no-naked-return`, `no-os-exit-outside-main`, `no-global-mutable-state` and
`no-bare-goroutine` — all of which real codebases reject. A probe set where
everything passes proves the method does not discriminate. There is a test
asserting those four stay in.

**Do not tune thresholds to get the answer you want.** `HeldBelow` sits at 0.15
because `panic` in library code measured 0.11 and 0.05 in two codebases that
clearly avoid it. If a candidate lands just the wrong side of a threshold, that
is information, not a reason to move the line.

---

## Release checklist

1. `go test ./...`
2. `strata promote-check --evidence evidence/` — review every non-promote outcome
3. Demote anything that has fallen below the bar, and write down why
4. `ratchet scan . && ratchet check .` — the tool holds its own line, or it has
   no standing to hold anyone else's
5. Update `rules/core/go/README.md` if the evidence table has moved
