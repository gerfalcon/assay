# Architecture

assay is a set of small tools over a shared data format. The structure follows
from that: the format is the only thing every tool agrees on, so it must not
depend on any of them.

Three rules, in order of how much they would cost to get wrong.

**The schema depends on nothing.** `pkg/schema` is the published contract. It is
the one package another organisation's tool might import, and a tool in another
language must be able to reimplement it from the record types alone. If it
reached into `internal/`, the contract would quietly acquire our implementation.

**The tools do not import each other.** `ratchet`, `strata`, `lens`, `docket`,
`plumb` and `judge` compose over JSONL, not over Go symbols. A dependency between two of
them would make the composability claim false in the one place it is easiest to
check, and would mean installing one pulled in another.

**Analysis does not depend on presentation.** `internal/analyze` measures;
`internal/report` renders. If measurement imported rendering, a change to output
formatting could alter a number, and the trend series would move for a reason
nobody could see.

<!-- The block below is ENFORCED by `make arch`. It is not decoration.
     Editing it is an architectural change: `plumb diff` classifies it, and a
     loosening needs a second reviewer and an entry under Why. -->

```arch
layer schema      pkg/schema
layer analysis    internal/analyze internal/arch internal/learn internal/judge
layer presenting  internal/report
layer internals   internal/baseline internal/store internal/docket internal/evidence internal/model internal/sarif internal/verdict

forbid schema    -> analysis
forbid schema    -> presenting
forbid schema    -> internals
forbid analysis  -> presenting
```

## Why

**2026-09-20.** Written when `plumb` was built, from three rules the codebase
already followed — so this records an existing shape rather than imposing a new
one. A declaration that fails on the day it is written teaches everyone to
ignore it.

The tools-do-not-import-each-other rule is not expressible here yet: `plumb`
covers package dependencies, and `cmd/*` are separate mains that the import
graph already keeps apart. It is checked by the fact that each binary builds
alone. If that ever stops being true, add a layer per command.
