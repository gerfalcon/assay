# Evidence

Aggregate precision data contributed by organisations. Counts only — no paths,
no fingerprints, no repository names, no code.

```
evidence/<rule-id>/<org>-<period>.jsonl
```

## Submitting

```sh
strata export --org acme --min-judged 10 > acme-2026-Q3.jsonl
strata verify acme-2026-Q3.jsonl          # check before it leaves your network
```

Read the file. It is small and it is meant to be read — that is the point of
exporting counts rather than a database dump.

Then open a PR adding it under `evidence/<rule-id>/`. CI runs the same verifier
on arrival.

## What the verifier enforces

It is an **allowlist**. Only these keys are permitted:

```
schema  rule  org  period  repos  fixed  carried  falsePositive  judged  precision
```

Anything else is rejected — including fields a future version of the exporter
might add. That asymmetry is deliberate: the dangerous failure is a new field
slipping past an old verifier, not an old field being over-scrutinised.

It also rejects a permitted field carrying something it should not — a rule id
that looks like a path, an org containing a ticket reference or an email, a
period finer than a quarter.

And it warns without blocking on thin evidence: fewer than 10 judged findings, or
a single repository. Partial evidence beats none, but you should know it is
partial — and small counts paired with an org name are more identifying than
large ones.
