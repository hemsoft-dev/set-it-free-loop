# Source repository SFL retirement

The source repository no longer runs its own consumer SFL deployment. Its
reviewer observer, autonomous workflows, generated maintenance and root deployment
manifest are removed under [issue #139](https://github.com/hemsoft-dev/set-it-free-loop/issues/139).
The organization App replacement is limited to the hs-buddy pilot on mini.
Native Codex remains the review engine; ordinary validation/release workflows remain.

The former root manifest is preserved as `deployment/legacy-source-manifest.json`
for release compatibility, outside the installed-manifest paths. Version updates
and release contract tests use that catalog without recreating an installation.

Historical compiled source workflows are retained outside `.github/workflows`
as immutable test fixtures for the migration ledger. `deploy-workflow.ps1 -Local`
now refuses before materialization or GitHub access, including dry runs.

Legacy deployment templates and CLI compatibility code remain available for
historical consumers. They are not an instruction to reinstall repository SFL
workflows. Template tests still validate compatibility, while asserting that the
consumer observer is absent from this repository. Existing consumer records,
labels, credentials and migration evidence remain.

Removed source deployment paths:

- `.github/workflows/sfl-dispatcher.yml`
- `.github/workflows/sfl-auditor.md`
- `.github/workflows/sfl-auditor.lock.yml`
- `.github/workflows/daily-repo-status.md`
- `.github/workflows/daily-repo-status.lock.yml`
- `.github/workflows/repo-audit.md`
- `.github/workflows/repo-audit.lock.yml`
- `.github/workflows/issue-processor.md`
- `.github/workflows/issue-processor.lock.yml`
- `.github/workflows/simplisticate.md`
- `.github/workflows/simplisticate.lock.yml`
- `.github/workflows/sfl-pr-review-auto.yml`
- `.github/workflows/pr-analyzer-general.md`
- `.github/workflows/pr-analyzer-general.lock.yml`
- `.github/workflows/pr-analyzer-quality.md`
- `.github/workflows/pr-analyzer-quality.lock.yml`
- `.github/workflows/pr-analyzer-security.md`
- `.github/workflows/pr-analyzer-security.lock.yml`
- `.github/workflows/pr-analyzer-testing.md`
- `.github/workflows/pr-analyzer-testing.lock.yml`
- `.github/workflows/pr-fixer.md`
- `.github/workflows/pr-fixer.lock.yml`
- `.github/workflows/pr-promoter.md`
- `.github/workflows/pr-promoter.lock.yml`
- `.github/workflows/agentics-maintenance.yml`
- `sfl.json`
