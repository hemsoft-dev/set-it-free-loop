# SFL v0 review presentation

pmd publishes reviews as `hemsoft-sfl-reviewer[bot]`. Each result names the
actual model and effort recorded for its review session.

Immediately below the verdict heading, the same line shows reviewer confidence
and project risk:

```text
**Confidence:** N/10 · **Project risk:** N/10 (slug)
```

Confidence is the reviewer's integer score from 0 to 10 for merging the PR as is:
10 means merge now, and 0 means must not merge. Project risk is a separate pmd
project level from 0 to 10, read just before publication using the canonical
repository name. No matching project, multiple matches, a non-integer or
out-of-range level, an empty slug, or a read failure shows
`**Project risk:** unknown`; the review still publishes. Both scores are display
only. Findings alone set the verdict, and neither score gates merging. Receipts
keep confidence alongside the verdict but do not store project risk.

The Full-Spectrum Review Summary groups findings into Security, Accuracy, and
Quality & Maintainability. Its severity table includes critical, high, medium,
and low counts, with explicit zeros on clean reviews. Numbered findings give
source locations and explain the trigger and impact. Review time and token
usage appear when the session provides them; missing usage is unavailable.

Coverage has five ordered groups: Files reviewed, Context read, Verified,
Not verified, and Tests run. Each nonempty group uses nested bullets so paths
and checks remain separate. Empty optional groups say `None reported.` The
parent adds one Tests run disclaimer because reviewer execution is disabled.

The owner requests a review with exactly `@sfl review` on an open PR with a
same-repository head. SFL covers every public or private hemsoft-dev repository
accessible to the App installation. Requests queue FIFO by numeric repository ID, PR and
head, with duplicates collapsed. There is no daily limit. Provider pause/hot
verdicts still apply, and SFL sessions have a separate concurrency limit from
Chief execution sessions. These changes do not activate parked SFL modules.

The implementation and operating contract live in
[project-manager's SFL v0 documentation](https://github.com/hemsoft-dev/project-manager/blob/main/docs/sfl-v0.md).
