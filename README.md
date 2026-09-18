# ratchet

Measure Go code quality, and hold the line against regression.

A ratchet turns one way. That is the whole design: record what is wrong today,
tolerate it, and fail only on what gets **worse**.

## Why this exists

Two claims sit behind this tool.

The first is that AI-generated code degrades maintainability over time, and that
this is a model training issue rather than a prompting one — coding-agent RL
rewards "the test passed and nothing else broke", and nothing in that objective
penalises poor design. The artefacts are predictable: casts added to satisfy a
checker, defensive wrapping that adds no value, errors acknowledged then dropped.

The second is that if you can **detect and measure** degradation, you can work on
it. That is what this tool is for.

It produces two outputs on purpose:

1. **Measurement** — per-function metrics as JSON, cheap enough to replay over
   git history and plot a trend.
2. **Action** — every finding carries a file, a line, the specific violation and
   a suggested fix. A number alone tells nobody what to do.

## Why a ratchet and not a gate

Turn a rule on across an existing codebase and you get four thousand violations,
and the team switches it off that afternoon. Big-bang conformance does not land.

So: `ratchet baseline` records current violations as tolerated, and
`ratchet check` fails only on new ones. The codebase can improve or hold, never
regress, and nobody has to stop and fix four thousand things first.

Generalised from ArchUnit's `FreezingArchRule`, including its safety property —
the baseline cannot be silently regenerated, or "fix the failure" quietly becomes
"rewrite the baseline".

## Install

```sh
go build -o ratchet ./cmd/ratchet
```

No third-party dependencies. Stdlib `go/ast` only.

## Use

```sh
ratchet scan .                    # measure and report
ratchet scan . --format json      # the trend record
ratchet scan . --format sarif     # GitHub PR annotations
ratchet baseline .                # record today as tolerated
ratchet check .                   # exit 1 only on regression
ratchet check . --strict-caps     # also fail if peak complexity grows
ratchet baseline . --tighten      # lock in what has been fixed
ratchet history . --since "6 months ago" --interval "1 week"
ratchet rules                     # what it checks and why
```

## Metrics

| Metric | What it tells you |
|---|---|
| cyclomatic | independent paths — branch density |
| cognitive | how hard it is for a *person* to follow |
| maxNesting | depth; usually rises before the others do |
| statements | size, counted in AST nodes so formatting cannot move it |
| params / results | signature width |

**Cyclomatic and cognitive are not the same metric twice.** A flat twenty-case
switch scores high on cyclomatic and low on cognitive: many paths, nothing to
hold in your head. Three nested ifs score the reverse. There is a test asserting
they diverge, because implementing the same thing twice under two names is the
easy mistake here.

Distributions (p50/p90/max) are reported rather than means. A mean hides the
tail, and the tail is the thing worth fixing — one unmaintainable function among
a thousand tidy ones barely moves an average.

## Rules

| Rule | Severity | What it catches |
|---|---|---|
| `naked-type-assertion` | error | `x.(T)` without comma-ok — panics at runtime. The Go analogue of a cast added to satisfy a checker |
| `error-swallowed` | error | `_ = err`, empty error branches, `return nil` when err is non-nil |
| `any-in-exported-signature` | warn | pushes type checking to runtime and onto the caller |
| `panic-in-library` | warn | removes the caller's ability to decide |
| `else-after-return` | info | nesting for no reason |

Every rule is individually toggleable via `--rules`. A rule you cannot switch off
is a rule that gets the whole tool switched off.

## Design decisions worth knowing

**No type information.** No `go/types`, no package loading. That costs precision
— error detection is name-based heuristics — and buys the ability to replay over
thousands of historical commits, many of which will not compile with the current
toolchain. Files that fail to parse are skipped rather than fatal.

**Fingerprints exclude line numbers.** Keyed on file, rule, enclosing function
and a normalised snippet. If a fingerprint moved whenever someone reformatted,
the first `gofmt` after adoption would read as a wave of new violations and the
gate would be switched off within a week. There is a test for this.

**Measure the derivative, not the level.** Nobody will agree that complexity 12
is bad. Everybody will agree that peak complexity went from 12 to 40 in four
months.

**Never gate on an aggregate score.** Any metric used as a gate on a
probabilistic optimiser gets gamed — gate on complexity and an agent will split
functions arbitrarily to satisfy it. So the default gate is the fingerprint set
(new violations only); aggregate caps are advisory unless you pass
`--strict-caps`, and even then they are maxima on the tail, not averages.

**Tests assert exact numbers.** A metric whose definition drifts silently is
worse than no metric: the trend line stays smooth while its meaning changes
underneath, and every historical datapoint becomes a lie.

**Tests, vendor and generated code are excluded by default.** Test code has
different norms, vendored code is not ours to fix, and generated code changes
en masse — any of the three would dominate a trend line.

## It holds its own baseline

ratchet runs on ratchet in CI. If it cannot hold its own line it has no standing
to hold anyone else's.

The first run reported `maxNesting` at cognitive 52 — the worst function in the
codebase, and one of its own metric implementations. Extracting the duplicated
switch/select traversal took peak cognitive complexity from **52 to 41**, peak
nesting from **4 to 3**, and mean cognitive from **6.06 to 4.72**, with `check`
confirming no regression. That loop — measure, locate, fix, re-measure, verify —
is the entire point.

## What it does not do

- No cross-file or whole-program analysis. Single-file AST only.
- No architecture conformance (layering, allowed dependencies). Go's compiler
  already forbids import cycles, so the useful gate there is a declared layering
  rule — see `go-arch-lint`.
- No change coupling. That comes from git history, not source, and `scc` already
  does it well in one pass.
- No autofix yet. Suggestions are text.

## Roadmap

Tracked as `talanet-abl` and children. Next up: SARIF wiring into CI, the
two-run eval harness with a metric holdout, and autofix for the mechanically
safe rules.
