# HemSoft SFL Reviewer Parity TODO

This is the active backlog for bringing the private HemSoft distribution of the
SFL pull request reviewer to operational parity with
`relias-engineering/set-it-free-loop` while preserving HemSoft's intentionally
different model provider and private-repository boundary.

The comparison baseline is Relias SFL `v6.5.7` at tag commit
`8d5e30714fa6cc61a89189266f8eb463132abde9`, reviewed through default-branch
commit `669d4d84ef37ebab107c5931d6727846626062e5`. Only `TODO.md` changed between
those commits. Recheck the Relias default branch before starting each parity
item because that baseline can move.

## Definition of parity

Parity is complete when all of the following are true:

- Reviewer safety, evidence, approval, recovery, and finding-lifecycle
  contracts match the current Relias reviewer unless an intentional HemSoft
  difference is documented and tested.
- A fresh machine can install a versioned, checksum-verified HemSoft `gh sfl`
  CLI without depending on a local Relias checkout.
- `gh sfl init`, `sync`, and `status` operate against immutable HemSoft releases
  and default to deployment pull requests.
- Every approved private HemSoft consumer can be inventoried, upgraded, and
  checked for drift from one documented rollout path.
- One private pilot repository passes a complete review, recovery, finding
  resolution, and required-gate smoke test.

Model identity is not a parity requirement. HemSoft intentionally uses Kimi K3
through its private OpenRouter route; Relias uses its organization Copilot
configuration. The review and security contracts must remain equivalent even
when provider-specific configuration differs.

## Verified baseline — 2026-08-13

Already present:

- [x] Private distribution source at `HemSoft/set-it-free-loop`.
- [x] Deployment mutations reject repositories outside private `HemSoft/*`
  scope.
- [x] Reviewer source, compiled lock, automatic trigger/gate, and recovery
  workflow are maintained in the source repository.
- [x] Deployment through a pull request is supported by
  `deployment/scripts/deploy-workflow.ps1`.
- [x] Consumer manifests retain an immutable source commit and preserve
  components from an existing larger SFL installation.
- [x] Repository-scoped GitHub App credential and review-gate setup helpers
  exist.
- [x] Issue #40 / PR #41 brought most reviewer behavior to the Relias v6.5.7
  contract.
- [x] Issue #21 configures explicit Kimi K3 pricing in both the primary reviewer
  and mandatory threat-detection runtimes; contract coverage rejects a lock
  that prices only one runtime.

Known gaps:

- [x] HemSoft validates GitHub's `isOutdated` flag before resolving an
  obsolete SFL review thread, matching Relias v6.5.7.
- [x] Release metadata distinguishes HemSoft distribution `2.1.0-rc.3` from the
  pinned Relias reviewer release and immutable reviewed commit.
- [x] Private prerelease `v2.1.0-rc.3` is an immutable, signed GitHub release at
  commit `c8ae5e8da61eccedc005c3e37c0818319e16cb04`; workflow run `31704942136`
  verified every asset and a clean private install.
- [x] `.github/workflows/publish-private-prerelease.yml` parses and has a
  checksum-pinned actionlint gate; publication is manual and prerelease-only.
- [x] The HemSoft CLI source is version-controlled under `gh-sfl/`; its build,
  tests, and installer have no dependency on a local Relias checkout.
- [x] CI formats, vets, tests, and cross-builds the CLI for Windows and Linux.
- [ ] No private consumer currently has the complete current reviewer package
  and current manifest.
- [ ] Historical reviewer artifacts exist in public HemSoft repositories even
  though the supported distribution boundary is now private-only.
- [x] Reviewer canonical/staged contract assertions normalize CRLF, LF, and CR
  while retaining case-sensitive content-drift detection.

## Required work

Work top to bottom. Do not declare rollout parity from source-only tests; each
phase has evidence that must exist before it is complete.

### P0 — Close reviewer contract drift

- [x] Port Relias's obsolete-thread guard: query `isOutdated` and refuse to
  resolve a live unresolved thread.
- [x] Add regression cases proving that an outdated SFL thread may be resolved,
  an already resolved thread is idempotent, and a live unresolved thread is
  rejected.
- [x] Compare all reviewer source/runtime artifacts with current Relias `main`.
  Document every remaining difference as one of:
  - HemSoft OpenRouter/Kimi configuration;
  - private HemSoft repository enforcement;
  - a tested HemSoft hardening that is at least as strict as Relias.
- [x] Regenerate the reviewer lock with the repository's checksum-verified
  `gh-aw` compiler; never edit the generated lock directly.
- [x] Pass the reviewer platform, verdict, recovery, and workflow contract
  suites plus `actionlint` for the generated and wrapper workflows.

Evidence required: a contract-diff note, green focused tests, compiler version
and checksum, and canonical/runtime artifact parity.

### P0 — Restore a trustworthy release pipeline

- [x] Replace the unsafe automatic version workflow with a manually dispatched,
  fail-closed prerelease workflow and checksum-pinned actionlint gate.
- [x] Make release metadata distinguish the HemSoft distribution version from
  the Relias reviewer baseline version and immutable baseline commit.
- [x] Keep `VERSION`, `sfl.json`, tags, release notes, and CLI artifacts in sync
  from one release command or workflow.
- [x] Generate SHA-256 checksums for every published CLI artifact and verify
  them during installation.
- [x] Protect release tags from movement or replacement using the strongest
  repository-level control available to the HemSoft account.
- [x] Publish one private prerelease and prove installation from its immutable
  tag before publishing the first stable release.

Evidence required: green release workflow, immutable tag, private GitHub
release, checksums, and a clean-machine install log.

### P0 — Make `gh sfl` independently distributable

- [x] Remove the runtime/build dependency on a local Relias checkout. Maintain
  the HemSoft adaptation from version-controlled source or an immutable,
  checksum-verified upstream input.
- [x] Preserve the hard gates for active GitHub identity `HemSoft`, owner
  `HemSoft`, and repository visibility `PRIVATE` on every mutating command.
- [x] Make reviewer-only installation the default tier; require an explicit
  option for the legacy full suite.
- [x] Resolve workflow content from an immutable HemSoft release and record its
  version and source SHA in `.sfl/sfl.json`.
- [x] Default `init` and `sync` to a deployment PR. Keep direct mutation an
  explicit opt-in and reject it for protected targets where it is unsafe.
- [x] Ensure `status` compares the installed manifest and managed files with the
  latest synchronized HemSoft release, not merely with a mutable branch.
- [x] Reconcile obsolete managed files without deleting consumer-owned files,
  and roll back partial mutations when deployment fails.
- [x] Test `init`, `sync`, `status`, dry-run, protected-repository behavior,
  uninstall/reconciliation, authentication failures, and private-scope
  rejection.

Evidence required: installation on a machine without the Relias source tree,
green CLI tests, and an immutable-source deployment PR.

### P1 — Make review gating part of the rollout contract

- [x] Preflight the required App installation, `SFL_APP_CLIENT_ID`,
  `SFL_APP_PRIVATE_KEY`, OpenRouter credential, Actions permissions, and default
  branch before opening a deployment PR.
- [ ] After the deployment PR merges, configure the native
  `SFL Reviewer Approval` required check without replacing unrelated branch
  protection settings.
- [ ] Make gate setup idempotent and restore the previous protection state if
  an update fails.
- [x] Have `gh sfl status` report missing credentials, missing required gate,
  stale manifest, file drift, and absence of a successful reviewer run as
  distinct failures.
- [x] Document the trusted boundary: maintainers who can change workflows on the
  protected default branch are trusted in this first HemSoft model.

Evidence required: before/after protection snapshots, idempotent rerun, rollback
test, and a status report that detects each deliberate fault.

### P1 — Converge approved private consumers

- [ ] Create an explicit allowlist of approved private HemSoft consumers. Do not
  infer approval from every repository visible to the account.
- [ ] Inventory each approved consumer's manifest, managed files, source SHA,
  credentials, required gate, and latest successful reviewer run.
- [ ] Select one private pilot repository and deploy the complete current
  reviewer package through a PR.
- [ ] Smoke-test automatic review, explicit rerun, immutable base/head checks,
  inline findings at every severity, recovery retry limits, obsolete-thread
  resolution, and the zero-finding approval gate.
- [ ] Roll the same release to the remaining allowlisted repositories in bounded
  batches, stopping on the first failed repository.
- [ ] Produce a convergence report showing release, source SHA, managed-file
  parity, gate state, and smoke-test state per consumer.
- [ ] Audit public HemSoft repositories and remove obsolete SFL reviewer
  artifacts through ordinary reviewed PRs. Never deploy the private HemSoft
  reviewer package or its credentials to a public repository.

Evidence required: one successful private pilot, a reviewed PR per mutation,
and a zero-drift convergence report for all approved consumers.

### P1 — Prevent future Relias drift

- [x] Record the last reviewed Relias tag and commit in machine-readable
  metadata.
- [x] Add a read-only parity audit that compares the HemSoft reviewer package
  with the recorded Relias baseline and permits only documented differences.
- [ ] Require a deliberate baseline update when Relias changes reviewer safety,
  recovery, gate, CLI, or release behavior.
- [ ] Make parity audit failures create one deduplicated tracking issue rather
  than silently changing HemSoft workflows.
- [x] Keep Windows and Linux contract tests line-ending independent.

Evidence required: a fixture proving an unapproved upstream safety change fails
the audit and an approved provider-only difference passes.

## Definition of done

All required work above is complete, and:

- [ ] The HemSoft source checkout and GitHub default branch are clean and at the
  same commit.
- [ ] The release, CLI, workflow, security, and contract-test suites pass from a
  clean checkout.
- [x] The latest private HemSoft release can be installed without local Relias
  files and verifies all checksums.
- [ ] Every allowlisted private consumer is on that immutable release with no
  managed-file drift and a required `SFL Reviewer Approval` gate.
- [ ] At least one current consumer has end-to-end smoke evidence for review,
  recovery, finding lifecycle, and approval.
- [ ] Documentation describes the supported install, update, status, rollback,
  and removal paths.

## Deferred until required parity is complete

These are useful scale or product improvements, but they are not required to
match Relias's supported manual `gh sfl init` rollout and must not block the
first strong HemSoft release:

- An unattended central deployment broker.
- A second publisher/deployer GitHub App or OIDC-based publisher boundary.
- Automatic rollout to every private HemSoft repository.
- Full-suite workflow expansion, feature-intake normalization, cost reporting,
  and onboarding-health workflows unrelated to reviewer parity.

Any proposal to add a broker, another App, or broader repository scope requires
an explicit design decision before implementation.

## Reference implementation

- Relias reviewer source: `D:\github\Relias\set-it-free-loop\workflows`
- Relias CLI source: `D:\github\Relias\set-it-free-loop\gh-sfl`
- Relias rollout documentation:
  `D:\github\Relias\set-it-free-loop\docs\SFL-REVIEWER-APP.md`
- HemSoft reviewer contract: [docs/SFL-REVIEWER.md](docs/SFL-REVIEWER.md)
- HemSoft deployment script:
  [deployment/scripts/deploy-workflow.ps1](deployment/scripts/deploy-workflow.ps1)
- HemSoft gate script:
  [deployment/scripts/set-sfl-review-gate.ps1](deployment/scripts/set-sfl-review-gate.ps1)
- Latest private release proof:
  [docs/release-evidence/v2.1.0-rc.3.md](docs/release-evidence/v2.1.0-rc.3.md)

The local Relias paths are comparison conveniences only. HemSoft build,
installation, and deployment must not depend on those paths.
