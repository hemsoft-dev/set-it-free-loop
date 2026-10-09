# Set it Free Loop™

[![SFL](https://img.shields.io/badge/SFL-2.1.0--rc.1-FFD700?style=flat-square)](CATALOG.md)
[![Tier](https://img.shields.io/badge/tier-full-0e8a16?style=flat-square)](CATALOG.md)
[![Workflows](https://img.shields.io/badge/workflows-9-blue?style=flat-square)](CATALOG.md)

> **One Intake. One Loop. Compounding Quality.**

The Set it Free Loop is a continuous quality improvement operating model for software repositories.
It converts quality signals and feature requests into governed, measurable software delivery — automatically.

## Organization PR reviewer

The reviewer is moving to the organization-installed SFL GitHub App and one
central service on mini. Consumer repositories need no SFL Actions workflow or
SFL model/App credential. Native Codex remains the review engine.

The only authorized qualification target is `hemsoft-dev/hs-buddy`. Organization
SFL workflows remain paused. See the [central reviewer runbook](central-reviewer/README.md)
and [tracking issue #139](https://github.com/hemsoft-dev/set-it-free-loop/issues/139)
for setup, scope, live evidence and remaining work. Broader activation requires a
separate owner instruction.

---

## What this repo is

This is the **canonical home** of the Set it Free Loop operating model. It contains:

| Folder | Purpose |
|--------|---------|
| `.github/workflows/` | **Staging** — workflows run on *this repo* first (dogfooding + verification) |
| `deployment/` | **Production** — everything needed to onboard a consumer repo |
| `deployment/workflows/` | Graduated workflow library, ready to deploy |
| `deployment/governance/` | Label taxonomy, policy, and setup scripts |
| `deployment/scripts/` | Multi-repo deployment tooling |
| `.github/prompts/` | VS Code Copilot intake prompts |
| `docs/` | Maintainer reference (sfl: format spec) |

---

## How the loop works

```
Scheduled workflow runs
        │
        ▼
Creates a GitHub Issue
        │
        ├──► type:report       → Human reads it. Loop stops here.
        │
        └──► type:action-item  → Enters the loop ──►  AI Processor
                                                            │
                                                    Opens a Pull Request
                                                            │
                                                    Reviewed & merged
                                                            │
                                                    Code improves ──► loop repeats
```

See [SOLVING-SOFTWARE-ENGINEERING.md](SOLVING-SOFTWARE-ENGINEERING.md) for the full operating model playbook.

---

## Historical per-repository deployment (retired)

The instructions below describe the previous architecture. Do not run them to
install the PR reviewer; use the central App runbook above. Autonomous workflows
remain paused.

### 1. Set up labels

```powershell
.\deployment\governance\setup-labels.ps1 -Owner <org> -Repo <repo>
```

### 2. Deploy a tier (or single workflow)

```powershell
# Deploy the full autonomous loop
.\deployment\scripts\deploy-workflow.ps1 -Tier full -Repos "hemsoft-dev/repository"

# Or deploy the subscription-backed Codex pull request reviewer
.\deployment\scripts\deploy-workflow.ps1 -Tier review -Repos "hemsoft-dev/repository"

# Or start with hygiene-only
.\deployment\scripts\deploy-workflow.ps1 -Tier minimal -Repos "org/repo"

# Or deploy a single workflow
.\deployment\scripts\deploy-workflow.ps1 -Workflow repo-audit -Repos "org/repo1,org/repo2"
```

### 3. Review the catalog

See [CATALOG.md](CATALOG.md) for all available workflows, tiers, and expected outputs.
Reviewer installation, current-head evidence, and optional gating are documented in
[docs/SFL-REVIEWER.md](docs/SFL-REVIEWER.md).

For the private HemSoft CLI path, install a checksum-verified release with
`.\deployment\scripts\install-gh-sfl-hemsoft.ps1 -ReleaseVersion <version>` or
build repository-owned source with `.\gh-sfl\build.ps1 -NoInstall`. No local
Relias checkout is required. `gh sfl init --repo hemsoft-dev/repository`
defaults to the reviewer-only tier and opens a deployment pull request.

---

## Versioning

The SFL uses [Semantic Versioning](https://semver.org/). The single source of
truth is the [`VERSION`](VERSION) file in this repo. The synchronized
[`deployment/release-metadata.json`](deployment/release-metadata.json) records
the HemSoft distribution version and native Codex reviewer identity.

Organization deployment, selected App credentials, and the source cutover are
documented in [docs/ORGANIZATION-DEPLOYMENT.md](docs/ORGANIZATION-DEPLOYMENT.md).

### Private release contract

Releases are deliberately manual. A version change is prepared on a branch with
`deployment/scripts/set-release-version.ps1`, reviewed through a pull request,
and merged before `Publish Private Prerelease` may run on `main`. The workflow
fails closed unless `VERSION`, the legacy source catalog, release metadata, the requested tag,
the default branch, and the immutable tag ruleset agree. It builds Windows and
Linux amd64 binaries, publishes `SHA256SUMS`, and proves a fresh authenticated
download through the installer. See [docs/RELEASING.md](docs/RELEASING.md).

Stable publication remains disabled until the prerelease and pilot evidence in
[`TODO.md`](TODO.md) is complete.

### How it works

| Repo | Version source | Badge shows |
|------|---------------|-------------|
| **This repo** (SFL source) | `VERSION` file | Latest released version |
| **Consumer repo** | `sfl.json` manifest | Version deployed to that consumer |

When you deploy to a consumer repo, the deploy script reads `VERSION`, stamps it into the consumer's `sfl.json`, and SHA-pins the source. To check if a consumer is current:

```
SFL VERSION file:  2.1.0-rc.3 ← selected release
Consumer sfl.json: 2.0.0      ← deployed version (behind)
```

### Adding the badge to a consumer repo

After deploying, add this to the consumer's README:

```markdown
[![SFL](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fraw.githubusercontent.com%2F<ORG>%2F<REPO>%2Fmain%2Fsfl.json&query=%24.version&label=SFL&color=FFD700&style=flat-square)](https://github.com/hemsoft-dev/set-it-free-loop)
```

Replace `<ORG>/<REPO>` with the consumer's GitHub org and repo name.

### Upgrading a consumer

Re-run the deploy script at the desired SFL version:

```powershell
# Pull latest set-it-free-loop, then:
.\deployment\scripts\deploy-workflow.ps1 -Tier full -Repos "org/repo"
```

The deploy script reads the current `VERSION`, creates a PR with updated workflow files, and stamps the new version into `sfl.json`.

---

## Workflow lifecycle

```
/new-workflow prompt in VS Code
    ↓
.github/workflows/{name}.md   ← STAGING (runs on this repo, verified here)
    ↓  acceptance criteria pass
deployment/workflows/{name}.md ← PRODUCTION (CATALOG.md entry, deployable)
```

Nothing enters `deployment/` without having run successfully in staging first.

---

## Adding a new workflow

1. Open VS Code in this repo
2. Run `/new-workflow` in GitHub Copilot Chat
3. Answer the guided questions
4. A draft appears in `.github/workflows/` — verify it runs
5. Once acceptance criteria pass, graduate it to `deployment/workflows/` and add a CATALOG.md entry

---

## References

- [CATALOG.md](CATALOG.md) — ready-to-deploy workflow registry
- [TODO.md](TODO.md) — active reviewer parity backlog
- [docs/SFL-REVIEWER-PARITY.md](docs/SFL-REVIEWER-PARITY.md) — pinned Relias reviewer baseline and read-only parity audit
- [VISION.md](VISION.md) — strategic vision
- [SOLVING-SOFTWARE-ENGINEERING.md](SOLVING-SOFTWARE-ENGINEERING.md) — operating model playbook
- [deployment/governance/policy.md](deployment/governance/policy.md) — governance policy
- [docs/intake-format.md](docs/intake-format.md) — sfl: frontmatter spec (maintainers)
