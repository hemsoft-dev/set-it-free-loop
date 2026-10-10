# Source repository SFL retirement

SFL is frozen by owner instruction as of 2026-10-09. The source repository has
no active GitHub Actions workflows: all remaining validation, publishing and
credential workflows are removed from `.github/workflows` and retained only as
inactive regression fixtures in `deployment/tests/fixtures/retired-source-workflows`.
Repository Actions and historical workflow registrations are disabled.

The organization App installation and central mini reviewer service are retired.
The previous hs-buddy pilot is no longer active. Consumer SFL credentials and
configuration have been removed; native reviews and unrelated consumer CI remain
independent. The source implementation and historical records are retained for
reference. Do not deploy or reactivate SFL.

The earlier consumer retirement under
[issue #139](https://github.com/hemsoft-dev/set-it-free-loop/issues/139) removed the
reviewer observer, autonomous workflows, generated maintenance and root deployment
manifest. The details below preserve that historical implementation record.

The former root manifest is preserved as `deployment/legacy-source-manifest.json`
for release compatibility, outside the installed-manifest paths. Version updates
and release contract tests use that catalog without recreating an installation.

Historical compiled source workflows are retained outside `.github/workflows`
as immutable test fixtures for the migration ledger. `deploy-workflow.ps1 -Local`
now refuses before materialization or GitHub access, including dry runs.

Legacy deployment templates and CLI compatibility code remain available for
historical consumers. They are not an instruction to reinstall repository SFL
workflows. Template tests still validate compatibility, while asserting that the
consumer observer is absent from this repository. Historical consumer records and migration evidence remain. Live SFL
labels, configuration and credential copies are retired under the project freeze.

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
