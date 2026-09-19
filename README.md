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
| **`ratchet`** | the gate. `scan`, `import` (SARIF from any linter), `check`, `history` |
| **`strata`** | append-only history. `append`, `query`, `rollup`, `verdicts`, `stat` |
| **`lens`** | read a stream at a glance. `top`, `trend`, `diff`, `compare` |
| **`rulebook`** | *(not built)* rules + verdicts + measured precision |

Each is usable with the others absent. `strata` has nothing quality-specific in
it — point it at any conforming stream and range queries come back.

There is deliberately **no web UI**. `lens` covers reading the data from a
terminal; Grafana over the rollups or a static site from JSONL covers the rest.

```
$ lens trend --metric cplx.per_kloc --period month
service-c  ▅▇▇█▇▇▆▇▇▇▆▆▆▆▆▅▅▄▄▄▄▄▄▄▃▃▂▂▃▂▁▂▁▁▁▁▁▁▁▁▂▁▂▂▂▂▁▁▁▂▁▂▂▁▂▂▂▃▄▄▅▅▅  49.37 → 49.00
               2021-05-01 → 2026-07-01  (63 points)
```

**Full guide: [docs/USAGE.md](docs/USAGE.md)** — CI gating, SARIF from other
languages, where rules live, writing rules that are worth having, keeping
history, and which external tools to reach for.

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
go install github.com/sherzing/assay/cmd/lens@latest
```

Single Go module, one binary per `cmd/` — install only what you want. The shared
packages are an implementation convenience; the interop contract is the JSONL.

## It measures itself

assay runs on assay. The first self-scan put `Scan` at cognitive **41** — the
worst function in its own codebase — and `emitAssay`, written the same day, at
**28**. Both were genuinely doing too much.

Splitting them moved the codebase:

| | before | after |
|---|---|---|
| cognitive max | 41 | **27** |
| cognitive mean | 5.76 | **5.08** |
| cyclomatic max | 25 | **17** |
| nesting max | 4 | **3** |
| findings | 5 | **3** |

The two fewer findings were not the goal — they fell out of the restructure,
because the error paths that had been swallowed inside a walk closure became
honest return values once the function was split.

If it cannot hold its own line, it has no standing to hold anyone else's.
