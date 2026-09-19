# Core Go rules

Every rule here was derived from evidence, not from a style guide. The method:

1. Probe a candidate pattern against codebases judged healthy by other means.
2. **Near-zero in all of them** → the teams hold that convention. Candidate rule.
3. **Common in all of them** → they do not hold it. Not a rule, whatever the
   internet says.
4. **Divergent between them** → a matter of taste or age, not quality. Not a
   core rule; fine as an org rule.
5. A human confirms the absence is deliberate before anything ships.

## The evidence

Measured across two production Go services — one five years old, one three
months — totalling ~5,500 non-test files. Rate is occurrences per 1,000 lines.

| pattern | svc-a | svc-b | verdict |
|---|---|---|---|
| `err.Error()` string comparison | 0.00 | 0.00 | **held — shipped** |
| `_ = err` | 0.00 | 0.00 | **held — shipped** (already native) |
| `time.Sleep` in non-test | 0.02 | 0.02 | **held — shipped** |
| `init()` | 0.04 | 0.00 | **held — shipped** |
| `context.TODO()` | 0.06 | 0.00 | **held — shipped** |
| `panic()` in library | 0.11 | 0.05 | **held** (already native) |
| `fmt.Print*` in library | 0.30 | 0.00 | divergent — org rule |
| `%v` on error not `%w` | 0.50 | 0.00 | divergent — org rule |
| `interface{}` over `any` | **20.98** | 0.07 | divergent — age, not quality |
| `os.Exit` outside main | 0.35 | 1.21 | **not held — rejected** |
| goroutine without recover | 0.43 | 0.61 | **not held — rejected** |
| global mutable var | 0.65 | 1.59 | **not held — rejected** |
| naked return | **3.59** | **4.39** | **not held — rejected** |

## What the rejections tell you

`naked return` is on every Go style list. Both teams use it several times per
thousand lines. Shipping it as a rule would have produced thousands of findings
that nobody agrees are defects — the precise noise that gets a tool switched off.

`interface{}` vs `any` diverges by 300×, and the explanation is that one codebase
predates the `any` alias. That is an age artifact wearing the costume of a
quality signal. It belongs in an org rule for new code, never in core.

**This is why rules need evidence before they ship.** Three of assay's own five
original rules were noise on first contact with real code, and the pattern above
is how you find that out before anyone else does.

## Running them

```sh
semgrep --config rules/core/go --sarif | ratchet import - --mode check
```

Remember `--error`: `semgrep scan` exits 0 even with findings without it.
