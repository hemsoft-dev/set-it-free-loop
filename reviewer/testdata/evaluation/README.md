# Evidence-backed historical labels

These 20 candidates have **14 clean labels, 6 defect labels, and 0 unlabeled
cases**. Labels were recorded October 20, 2026 at 8:26 PM EDT by `/root`, OpenAI
`gpt-6.1-sol`, session `01a15233-b165-7fd3-9ba7-2caea2632bde`, with source
research by `/root/label_evidence_recent` and `/root/label_evidence_older`.
Every case and defect carries the final labeler's identity, timestamp, rationale
and evidence. No Franz override has occurred; histories are initially empty.

The source cutoff is
[`5dc0aa4d4bd1764ccc4882507f4600bce00d59ff`](https://github.com/hemsoft-dev/set-it-free-loop/commit/5dc0aa4d4bd1764ccc4882507f4600bce00d59ff).
No evaluated Codex or Claude review, finding, comment, or paraphrase was used as
label evidence. Source that handles reviews was inspected as code. Its fake
regressions are independent behavioral evidence. A review may have motivated a
later fix; the labels cite the actual corrective source and reproduced behavior.

## Corpus status

`labels.json` is a **draft** `EvaluationCorpus` v2. Its labels pass
`ValidateLabels`; packet hashes remain empty and packet sizes zero. It is not
scorable and has no corpus hash or frozen plan. The `FrozenAt` field supplies the
label-validation cutoff only until successful materialization. The full corpus
validator continues to reject it.

The real evidence producer cannot currently materialize candidate
[#156](https://github.com/hemsoft-dev/set-it-free-loop/pull/156): its 335 changed
paths exceed the existing 128-path inventory cap. No candidate was dropped or
replaced, and no limit was increased. Complete corpus freeze still requires a
supported case-selection or bounded-reader solution. This packet limitation
leaves no case *unlabeled*. It leaves the evaluation corpus unfrozen.

The freeze attempt also reproduced a packet-budget bug on
[#171](https://github.com/hemsoft-dev/set-it-free-loop/pull/171): late omission
records could overflow 64 KiB after earlier patches had fit. The source producer
now removes whole patches and their inline anchors until the complete inventory
and coverage fit. The new real-Git regression fails before that fix and passes
afterward, including restore of the same packet. The 64 KiB and 128-path limits
are unchanged.

## Search method and limits

The seven-calendar-day observation window was chosen before classifying cases.
Each clean label records its exact merge time, seven-day endpoint, immutable
history revision, searches and limits. For every candidate:

1. Inspect `git diff <base> <head>`, including changed executable lines.
2. Inventory **all** changed paths, then enumerate every later first-parent
   main commit touching those paths through the immutable cutoff. Inspect
   corrective hunks for a causal fix, revert or regression, both within seven
   days and in the longer available history. Same-file edits alone are not
   defects. Intentional retirement and later owner scope changes are not fixes.
3. Check the complete GitHub issue metadata catalog observed October 20 at
   8:21 PM EDT: 74 issues, one page, `hasNextPage: false`, requesting only
   number, title, dates, URL and labels. The only recent bug-labeled issue,
   [#167](https://github.com/hemsoft-dev/set-it-free-loop/issues/167), predates
   its corrective candidate [#168](https://github.com/hemsoft-dev/set-it-free-loop/pull/168).
   No later matching regression title or label was found. Issue bodies,
   comments and review fields were excluded because they may repeat the
   evaluated outputs. An unrelated issue body could still mention a defect.
4. Check the six defect line ranges against the actual candidate diff and
   independently replay each fail-before/pass-after regression. The removed
   guard for [#159](https://github.com/hemsoft-dev/set-it-free-loop/pull/159)
   is cited on its **base** line 7; the other affected ranges cite their heads.

`source-audit.json` retains exact revisions, changed-path inventories and later
commit lists. The issue metadata receipt is retained in `issue-metadata.json`. The agent
research reports and all 12 root-replayed regression executions are retained
with this mission's Git-local evidence. `replay-defects.py` reproduces the six
fail-before/pass-after pairs from immutable source with fake GitHub data. The table below links the primary source needed to check the labels.
The metadata catalog is observation-time evidence; it cannot be reconstructed
by querying today's mutable issue list.

Clean means no causal later correction was found within this stated search.
Most of the seven days had no runtime exposure: the owner retired automation
in [#170](https://github.com/hemsoft-dev/set-it-free-loop/pull/170) and froze it
in [#171](https://github.com/hemsoft-dev/set-it-free-loop/pull/171). These labels
do not establish operational safety or absence of out-of-band defects. Historical
PRs may have been seen by models or maintainers. Selection predates alpha tuning,
but model-unseen eligibility cannot be established. Historical receipt blobs in
[#156](https://github.com/hemsoft-dev/set-it-free-loop/pull/156) were excluded
from semantic label evidence because they may embed provider reviews; its clean
assessment covers later corrections to its introduced executable/docs behavior.

## Labels and causal evidence

| Candidate | Label | Independent source assessment |
| --- | --- | --- |
| [171](https://github.com/hemsoft-dev/set-it-free-loop/pull/171) | Clean | Freeze and automation removal had no correction during seven days. [Later offline owner exception](https://github.com/hemsoft-dev/set-it-free-loop/commit/372f563b0f6dc93c3080b62288d7c65b572b10ff) is new authority. |
| [170](https://github.com/hemsoft-dev/set-it-free-loop/pull/170) | Clean | Source retirement was intentional. The following freeze extends it; no causal correction found. |
| [169](https://github.com/hemsoft-dev/set-it-free-loop/pull/169) | Clean | Central pilot source had no later corrective patch; subsequent retirement is intentional. Runtime exposure was brief. |
| [168](https://github.com/hemsoft-dev/set-it-free-loop/pull/168) | Clean | Canonical destination correction had no subsequent matching regression or corrective change. |
| [165](https://github.com/hemsoft-dev/set-it-free-loop/pull/165) | Clean | Chained-retarget invalidation correction survives without later causal repair. |
| [164](https://github.com/hemsoft-dev/set-it-free-loop/pull/164) | Clean | Registry-free request verification survives without later causal repair. |
| [163](https://github.com/hemsoft-dev/set-it-free-loop/pull/163) | Clean | Verification-error invalidation survives without later causal repair. |
| [162](https://github.com/hemsoft-dev/set-it-free-loop/pull/162) | Clean | Required-status repair branch survives. Later ordinary-publication changes correct pre-existing sibling behavior. |
| [161](https://github.com/hemsoft-dev/set-it-free-loop/pull/161) | Defect | [Head lines 795–800](https://github.com/hemsoft-dev/set-it-free-loop/blob/31482a869638604a8d6bb52e2c9c24e9d9d51589/deployment/infrastructure/sfl-pr-review-auto.yml#L795-L800) admit quoted/malformed marker requests from the title alone. [Later fix #164](https://github.com/hemsoft-dev/set-it-free-loop/commit/104648b0a9f2709c15d2cfb6c9b7d9dc49e171b3) verifies comment intent. |
| [160](https://github.com/hemsoft-dev/set-it-free-loop/pull/160) | Defect | [Head lines 777–787](https://github.com/hemsoft-dev/set-it-free-loop/blob/a293479a7615e7379e4ccc67d95bcabd43b0d802/deployment/infrastructure/sfl-pr-review-auto.yml#L777-L787) miss a valid writer request before registry visibility. [Later fix #161](https://github.com/hemsoft-dev/set-it-free-loop/commit/afdf3bca1631b98ad87b9aa96ed87d455917e63b) holds the materializing request. |
| [159](https://github.com/hemsoft-dev/set-it-free-loop/pull/159) | Defect | Removal of the [base line-7 request guard](https://github.com/hemsoft-dev/set-it-free-loop/blob/73607a1b28beb31e794be6498e0d244866b45ab5/.github/workflows/sfl-pr-review-auto.yml#L7) turns unrelated administrator comments into active invalidations. [Later fix #160](https://github.com/hemsoft-dev/set-it-free-loop/commit/8c53c43a860a0471308f11501352a9f2b43c7efd) binds titles to registered IDs. |
| [158](https://github.com/hemsoft-dev/set-it-free-loop/pull/158) | Defect | [Head lines 1273–1276](https://github.com/hemsoft-dev/set-it-free-loop/blob/d20858e5416974992544ad6fe4ed11c36daf3eea/.github/workflows/sfl-pr-review-auto.yml#L1273-L1276) admit unsupported nondefault-to-nondefault retargets and can falsely block them. [Later fix #160](https://github.com/hemsoft-dev/set-it-free-loop/commit/8c53c43a860a0471308f11501352a9f2b43c7efd) requires departure from default. |
| [157](https://github.com/hemsoft-dev/set-it-free-loop/pull/157) | Defect | [Head lines 1247–1253](https://github.com/hemsoft-dev/set-it-free-loop/blob/8fba49c1b67d0d2c80e5bf9d2e5a4c549823d084/.github/workflows/sfl-pr-review-auto.yml#L1247-L1253) skip invalidation when leaving main. [Later fix #158](https://github.com/hemsoft-dev/set-it-free-loop/commit/73607a1b28beb31e794be6498e0d244866b45ab5) handles that departure. |
| [156](https://github.com/hemsoft-dev/set-it-free-loop/pull/156) | Clean | New transfer validators, corruption checks and onboarding-update behavior survive without causal repair. Receipt-content coverage is excluded; packet inventory exceeds the reader cap. |
| [155](https://github.com/hemsoft-dev/set-it-free-loop/pull/155) | Clean | New `readReviewArtifact` base provenance survives. Later requester fixes address inherited branches. |
| [154](https://github.com/hemsoft-dev/set-it-free-loop/pull/154) | Defect | [Head lines 634–642](https://github.com/hemsoft-dev/set-it-free-loop/blob/3614b31b5f9f27c2580c024925bb30a5829bdc0a/.github/workflows/sfl-pr-review-auto.yml#L634-L642) hide edited newer registrations from supersession. [Later fix #158](https://github.com/hemsoft-dev/set-it-free-loop/commit/73607a1b28beb31e794be6498e0d244866b45ab5) retains their raw history. |
| [153](https://github.com/hemsoft-dev/set-it-free-loop/pull/153) | Clean | Workflow-preserving full-sync implementation remains unchanged through cutoff. |
| [152](https://github.com/hemsoft-dev/set-it-free-loop/pull/152) | Clean | Added wider-pilot window/order validation functions remain unchanged. |
| [151](https://github.com/hemsoft-dev/set-it-free-loop/pull/151) | Clean | Coverage module and fake tests remain unchanged; later relocation retires the workflow. |
| [150](https://github.com/hemsoft-dev/set-it-free-loop/pull/150) | Clean | Base-advance recovery instruction survives; later wording changes affect a different branch. |

All six labeled defects are material because they can admit ambiguous review
history, preserve stale gate success, or block a required review incorrectly.
The twelve focused executions cover six failures and six corresponding fixes.
They are source-behavior evidence, not reviewer quality measurements.

## Reproduce validation and the freeze attempt

From the repository root, using the declared Go toolchain:

```sh
python3 reviewer/testdata/evaluation/replay-defects.py
go -C reviewer test -run 'TestRetainedEvaluationLabels|TestAgent|TestSnapshotEvidenceBudgetsCoverage' ./...
git_dir=$(git rev-parse --path-format=absolute --git-common-dir)
output_parent=$(mktemp -d)
go -C reviewer run ./testdata/evaluation/freeze "$git_dir" testdata/evaluation/labels.json "$output_parent/corpus"
```

The final command uses `CaptureSnapshotEvidence` against existing immutable Git
objects, with no checkout, hooks, external diff/textconv, fetch, credentials or
model execution. It currently fails at `pr-156` with the inventory-limit error.
It creates no output directory until **every** case materializes and the entire
corpus validates. After supported materialization, it writes exact packet bytes,
`corpus.json`, and `corpus.sha256` from the existing `EvaluationCorpus.Hash()`.
A new output directory prevents overwriting frozen artifacts.

Before a real freeze, recheck held-out eligibility and update the draft's cutoff
time after all labels. Freeze that complete corpus first, then freeze a separate
`EvaluationPlan` with its corpus hash, trusted release/config and later creation
time. Any revised label or owner override retains its prior label and creates
a new hash and plan. No plan, provider run, trusted-release promotion, App
installation or activation is supplied by these labels.
