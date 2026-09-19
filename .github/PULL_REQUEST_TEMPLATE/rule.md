## The rule

<!-- What does it catch, and why is that a defect rather than a preference? -->

## Evidence

<!-- Required. A rule without evidence is not reviewable.

Either the absence method:
  ratchet learn <repo> <repo>          # two or more codebases you trust

Or precision data from real use:
  strata precision --rule my-rule
-->

| codebase | rate or precision |
|---|---|
|  |  |

## Known false positives

<!-- Every rule has some. "None" means it has not been run hard enough.
     Name the classes so reviewers and users do not rediscover them. -->

## Checklist

- [ ] Run against a codebase **I did not write**, and I read every finding
- [ ] The message says what to do, not just what is wrong
- [ ] No existing linter already catches this
- [ ] `confidence` in the metadata is honest — `low` is a fine answer
- [ ] Two reasonable teams would agree this is a defect, not a style preference

## If this is an org rule rather than core

<!-- Core is for defects. If the answer to the last checkbox is "they might
     disagree", say so and propose it under rules/org/ instead. That is not a
     lesser outcome — most good rules are org rules. -->
