# assay

Small, independent tools for measuring code quality over time. They compose over
JSON Lines, following the Unix model: each does one thing, reads a stream, writes
a stream.

```sh
ratchet scan . --emit measures | strata append --repo service-a
golangci-lint run --out-format sarif | ratchet import - | ratchet check
strata query --repo service-a --metric cognitive.p90 --since 2026-01 --format csv
```

## Why

Sonar and Codacy charge per seat, largely for a *platform* — server, storage,
ingestion, dashboards. AI made building that kind of platform cheap. It did not
make **good rules** cheap: authoring a syntactically valid rule takes seconds,
knowing whether it is mostly noise still takes judgement and real codebases.

So the value is not the tool. It is the **rule corpus and the precision data
behind it** — the part that compounds, because it is accumulated judgement.

## The contract is the format

Composability comes from a shared data format, not from good module boundaries.
`ls | grep | wc` works because of text. Three versioned JSONL records:

| record | what it says |
|---|---|
| `finding` | something is wrong at this location |
| `measure` | this number, this commit, this scope (project/module/file/function) |
| `verdict` | a human judged this finding |

`schemas/*.json` is the specification. The Go types in `pkg/schema` are one
implementation — a conforming tool in any language composes with these.

## Tools

| | |
|---|---|
| **`ratchet`** | the gate. `scan`, `import` (SARIF from any linter), `check` |
| **`strata`** | append-only history. `append`, `query`, `rollup`, `verdicts` |
| **`rulebook`** | *(not built)* rules + verdicts + measured precision |

Each is usable with the others absent. `strata` has nothing quality-specific in
it — point it at any conforming stream and range queries come back.

There is deliberately **no web UI**. Grafana over the rollups, or a static site
from JSONL, covers it. Revisit only if its absence becomes a real complaint.

## The verdict split is the point

A single "tolerated" bucket conflates two different things:

| verdict | is it debt? | feeds rule quality? |
|---|---|---|
| `accepted` | yes — pay it down | no |
| `false-positive` | no — the rule is wrong | **yes** |
| `wont-fix` | no | no |

False-positive rate per rule becomes a **rule quality metric**. Rank rules by
measured precision, retire the noisy ones, publish the data. That is the record
vendors keep in their databases, and the reason this is worth building.

Motivating evidence: SmellBench (2026) found **63.1%** of detected
"hard-severity" architectural smells were expert-judged false positives. Three of
ratchet's own five original rules were noise against a real codebase, and only
running them revealed it.

## Rule tiers and promotion

```
rules/core/    published precision data, community-maintained
rules/org/     a company's own pack
rules/local/   project-specific, in the repo being analysed
```

Resolution is local → org → core, most specific wins. A rule is **promoted on
evidence**: run against ≥N repositories, ≥M verdicts, precision ≥0.8, judgements
from ≥2 organisations.

**Precision data is portable even when code is not.** An organisation can
contribute "47 findings, 41 confirmed, 6 false positives" for a rule without
sharing a line of source. That is what lets closed-source teams participate in an
open corpus.

## Storage: files

```
.assay/
  measures/2026/09/19.jsonl      date-partitioned, append-only
  findings/2026/09/19T1000-sha.jsonl
  verdicts/verdicts.jsonl        append-only log, last write wins
  rollup/                        derived cache, always rebuildable
```

No database. Raw data is the truth and rollups are a cache, so there is no state
that cannot be recomputed. Partitioning by date makes a range query a directory
listing — no index. And every file is readable with `grep` and `jq` without any
binary here, which is the durability property that matters when tools get
abandoned or relicensed.

Scale: ~12,500 records across three repos is 3 MB. Five repos over five years
with function-level detail lands in the low hundreds of MB.

## Install

```sh
go install github.com/sherzing/assay/cmd/ratchet@latest
go install github.com/sherzing/assay/cmd/strata@latest
```

Single Go module, one binary per `cmd/` — install only what you want. The shared
packages are an implementation convenience; the interop contract is the JSONL.
