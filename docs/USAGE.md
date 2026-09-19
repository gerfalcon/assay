# Using assay

Three small tools that compose over JSON Lines. Nothing here needs a server, a
database, or an account.

```
ratchet   measure code, gate CI          scan · import · check · baseline · history
strata    keep the history               append · query · rollup · verdicts · stat
lens      make a stream readable         top · trend · diff · compare
```

---

## Install

```sh
go install github.com/sherzing/assay/cmd/ratchet@latest
go install github.com/sherzing/assay/cmd/strata@latest
go install github.com/sherzing/assay/cmd/lens@latest
```

One binary per tool — install only what you need. They share Go packages, but
the contract between them is the JSONL format, so you can replace any one of
them with your own implementation.

---

## Five minutes

```sh
# What does this repo look like?
ratchet scan .

# Record today as tolerated. Commit the file.
ratchet baseline .
git add .ratchet-baseline.json

# In CI: fail only if something got worse.
ratchet check .
```

That is the whole loop. Everything below is optional.

---

## Gating CI

`ratchet check` exits non-zero **only when a new finding appears.** Existing
violations are reported and tolerated.

This matters more than it sounds. Turn a linter on across a mature codebase and
you get four thousand violations, and the team switches it off that afternoon.
A ratchet lets the code improve or hold, never regress, without anyone stopping
to fix four thousand things first.

```yaml
# .github/workflows/quality.yml
- run: ratchet check . --strict-caps
```

| flag | effect |
|---|---|
| `--strict-caps` | also fail if peak complexity exceeds the baseline |
| `--rules a,b` | run only these rules |
| `--include-tests` | analyse `_test.go` too (off by default — test code has different norms) |
| `--json` | machine-readable result |

**The baseline cannot be silently regenerated.** `ratchet baseline` refuses to
overwrite an existing file; you need `--force` or `--tighten`. That is
deliberate — otherwise "fix the failing build" quietly becomes "rewrite the
baseline" and the gate stops meaning anything.

When you fix things, lock the improvement in:

```sh
ratchet baseline . --tighten     # drops only entries that no longer reproduce
```

---

## Any language, via SARIF

`ratchet` only parses Go. For everything else, use the language's own linter and
pipe its SARIF in — better fidelity than anything we would write, and no parsers
for us to maintain.

```sh
# Go
golangci-lint run --out-format sarif | ratchet import - --mode check

# C# — Roslyn analyzers
dotnet build -p:ErrorLog=roslyn.sarif
ratchet import roslyn.sarif --root . --mode baseline

# Dart
dart_code_linter analyze lib --reporter=json | your-shim | ratchet import -

# Anything semgrep covers
semgrep --config rules/ --sarif | ratchet import - --mode check
```

Three behaviours worth knowing:

- **Rule IDs are namespaced by tool.** Two linters both emitting `unused` will
  not collide in one baseline and silently excuse each other.
- **SARIF `suppressions` are honoured**, like `//nolint`. A tool that cannot be
  told no gets switched off entirely.
- **Fingerprints exclude line numbers.** A reformat must not read as a wave of
  new violations. The producer's own `partialFingerprints` are used when present.

---

## Where rules live, and how to customise them

### Built-in Go rules

```sh
ratchet rules      # what it checks and why
```

| rule | severity |
|---|---|
| `naked-type-assertion` | error |
| `error-swallowed` | error |
| `any-in-exported-signature` | warn |
| `panic-in-library` | warn |
| `else-after-return` | info |

Turn them on and off individually:

```sh
ratchet scan . --rules naked-type-assertion,error-swallowed
```

**Every rule must be individually switchable.** Some will be wrong for your
codebase, and a rule you cannot disable is a rule that gets the whole tool
disabled.

### Suppressing a single case

`//nolint` on the line, with a reason:

```go
s := x.(string) //nolint:forcetypeassert // validated by the caller
```

### Your own rules

Custom rules are **semgrep or ast-grep rule packs** — we do not reinvent a rule
language. Three tiers, most specific wins:

```
rules/local/      this project only, lives in the repo being analysed
rules/org/        your company's pack
rules/core/       shipped with assay, has published precision data
```

A rule is a YAML file:

```yaml
# rules/org/acme/no-direct-db-in-handler.yaml
rules:
  - id: no-direct-db-in-handler
    languages: [go]
    severity: ERROR
    message: HTTP handlers must not talk to the database directly
    paths:
      include: ["internal/httpapi/**"]
    pattern: $DB.Query(...)
```

```sh
semgrep --config rules/org --sarif | ratchet import - --mode check
```

> **Trap:** `semgrep scan` exits 0 *even with findings* unless you pass
> `--error`. Always assert the exit code, and test a new gate by planting a
> violation to confirm it actually fails.

### Writing rules that are worth having

AI will write you a syntactically valid rule in seconds. It cannot tell you
whether the rule is *right* for your codebase — that still takes judgement and
real code.

Three of assay's own five original rules turned out to be pure noise when first
run against a production service: variadic `...any` is the SQL-driver idiom, not
a smell; `panic` inside `must*` is the standard library's own convention. We only
found out by running them.

So: **write the rule, run it against a real repository, and read every finding
before you enable it.** If more than a few are wrong, fix the rule or drop it.

---

## Keeping history

```sh
ratchet scan . --emit measures --repo service-a | strata append
```

That is one point in time. For a trend, replay git history:

```sh
ratchet history . --interval week --first-parent --jobs 8 > history.jsonl
```

| flag | note |
|---|---|
| `--interval all\|day\|week\|month` | `all` measures every commit |
| `--first-parent` | **use this.** Walks the merge timeline — a feature-branch commit is work in progress, not a state the codebase was ever really in |
| `--jobs N` | checkout dominates the cost, so this scales nearly linearly |

Then query it:

```sh
strata stat
strata query --repo service-a --metric cognitive.p90 --since 2026-01-01 --format csv
strata rollup --period month --metric cognitive.p90
```

### What is on disk

```
.assay/
  measures/2026/09/19.jsonl      date-partitioned, append-only
  findings/2026/09/19T1000-sha.jsonl
  verdicts/verdicts.jsonl        append-only log, last write wins
  rollup/                        derived cache — safe to delete and rebuild
```

Raw data is the truth; rollups are a cache. Partitioning by date makes a range
query a directory listing, so there is no index to corrupt and no database to
run. Roughly 12,500 records is 3 MB.

Commit `.assay/` to git for a small repo, or sync it to object storage for an
org-wide history. The tools take a path and do not care which.

---

## Reading the results

`grep` and `jq` work — that is a deliberate property, not a substitute for
tooling. But for actually looking at something, use `lens`.

```sh
lens top --store .assay --n 10
```
```
worst 10 by cognitive (function scope)

     41 ██████████████████████  assay internal/analyze/analyze.go:Scan
     37 ███████████████████···  …helper.go:ValidationActionHelper.UpdateModelWithIncentiveMeta
     30 ████████████████······  …common/order_response_mapper.go:CreateTGOrderFailResponseModel
```

```sh
lens trend --store .assay --metric cplx.per_kloc --period month
```
```
service-c  ▅▇▇█▇▇▆▇▇▇▆▆▆▆▆▅▅▄▄▄▄▄▄▄▃▃▂▂▃▂▁▂▁▁▁▁▁▁▁▁▂▁▂▂▂▂▁▁▁▂▁▂▂▁▂▂▂▃▄▄▅▅▅  49.37 → 49.00  ▼ -1%
               2021-05-01 → 2026-07-01  (63 points)
```

The sparkline is the point: five years in one line, and the tail turning back up
is visible at a glance in a way no table makes obvious.

```sh
lens diff --store .assay --repo service-a --since 2026-01-01    # what moved
lens compare --store .assay --scope project                 # repos side by side
```

`lens` reads stdin too, so it works on any conforming stream:

```sh
ratchet scan . --emit measures | lens top --metric cognitive
strata query --repo service-a --metric cognitive | lens top --n 20
```

---

## Using external tools for more insight

assay deliberately does not reimplement things that already exist.

### Change coupling and hotspots — `scc`

The metric the research says actually predicts change- and error-proneness is
**co-change**, not structure. `scc` (MIT) does it across every language:

```sh
scc --coupling --depth 800              # files that change together
scc --coupling-for internal/order.go    # blast radius for one file
scc --by-file --cognitive --format json # complexity, any language
```

**Filter the output before believing it.** `degree = shared / union`, so two
files that each changed twice in the same two commits score 100%. Require at
least five shared commits, drop test↔subject pairs, and ignore pairs inside the
same module — those are cohesion, not shotgun surgery.

### Complexity for other languages

| language | tool | licence |
|---|---|---|
| many | `lizard` | MIT |
| Python | `wily` (per-commit history built in) | Apache-2.0 |
| Dart | `dart_code_linter` | MIT |
| C# | Roslyn analyzers via `ErrorLog=*.sarif` | MIT |

### Duplication

`jscpd` (MIT) or PMD CPD (BSD). Neither is built in.

### Architecture conformance

Declare your layering once, enforce it forever:

| language | tool |
|---|---|
| Go | `go-arch-lint`, or `depguard` inside golangci-lint |
| TS/JS | `dependency-cruiser` |
| Python | `tach`, `import-linter` |
| Java/Kotlin | ArchUnit — and use `FreezingArchRule`, which is the same ratchet idea |
| Dart | `import_rules` + `lakos` |

> Go forbids import cycles at the compiler, so a "no cycles" rule there is
> vacuous. Use a declared layering rule instead.

### Charts

Point Grafana at `.assay/rollup/` via CSV, or generate a static page from the
JSONL. There is deliberately no web UI here.

---

## Interpreting the numbers

**Cyclomatic and cognitive are not the same metric twice.** Cyclomatic counts
paths; cognitive models what a person must hold in their head — it penalises
nesting and charges `else` a flat 1. A flat twenty-case switch scores high
cyclomatic and low cognitive. Send people to fix the cognitive ones.

**Read p50 and p90, not max.** If only the max moved it is usually one function,
or just sampling — adding functions reaches further into the tail whether or not
anything decayed.

**Watch the derivative.** Nobody agrees that complexity 12 is bad. Everybody
agrees peak complexity going 12 → 40 in four months is bad.

**Never gate on an aggregate score.** Any metric used as a gate on a
probabilistic optimiser gets gamed — gate on complexity and an agent will split
functions arbitrarily to satisfy it. Gate on new findings; keep aggregates
advisory.

**Compare within a language only.** Branch-keyword density is a language trait:
Go's explicit `if err != nil` inflates complexity counts against C#'s exceptions.
Cross-language comparison measures the language, not the team.
