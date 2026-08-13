# Set it Free Loop™

[![SFL](https://img.shields.io/badge/SFL-2.0.0-FFD700?style=flat-square)](CATALOG.md)
[![Tier](https://img.shields.io/badge/tier-full-0e8a16?style=flat-square)](CATALOG.md)
[![Workflows](https://img.shields.io/badge/workflows-9-blue?style=flat-square)](CATALOG.md)

> **One Intake. One Loop. Compounding Quality.**

The Set it Free Loop is a continuous quality improvement operating model for software repositories.
It converts quality signals and feature requests into governed, measurable software delivery — automatically.

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

## Onboarding a consumer repo (3 steps)

### 1. Set up labels

```powershell
.\deployment\governance\setup-labels.ps1 -Owner <org> -Repo <repo>
```

### 2. Deploy a tier (or single workflow)

```powershell
# Deploy the full autonomous loop
.\deployment\scripts\deploy-workflow.ps1 -Tier full -Repos "HemSoft/private-repository"

# Or deploy the automatic, recoverable SFL pull request reviewer
.\deployment\scripts\deploy-workflow.ps1 -Tier review -Repos "HemSoft/private-repository"

# Or start with hygiene-only
.\deployment\scripts\deploy-workflow.ps1 -Tier minimal -Repos "org/repo"

# Or deploy a single workflow
.\deployment\scripts\deploy-workflow.ps1 -Workflow repo-audit -Repos "org/repo1,org/repo2"
```

### 3. Review the catalog

See [CATALOG.md](CATALOG.md) for all available workflows, tiers, and expected outputs.
Reviewer installation, recovery, and optional gating are documented in
[docs/SFL-REVIEWER.md](docs/SFL-REVIEWER.md).

For the private HemSoft CLI path, build the repository-owned source with
`.\gh-sfl\build.ps1 -NoInstall` or install it with
`.\deployment\scripts\install-gh-sfl-hemsoft.ps1`. No local Relias checkout is
required. `gh sfl init --repo HemSoft/private-repository` defaults to the
reviewer-only tier and opens a deployment pull request.

---

## Versioning

The SFL uses [Semantic Versioning](https://semver.org/). The single source of
truth is the [`VERSION`](VERSION) file in this repo. The synchronized
[`deployment/release-metadata.json`](deployment/release-metadata.json) keeps
that HemSoft distribution version separate from the pinned Relias reviewer
release and immutable reviewed commit.

### Automatic versioning

Automatic publication is currently safety-gated while the first HemSoft release
contract is completed. When the repository variable
`SFL_AUTO_VERSION_ENABLED` is explicitly set to `true`, versions bump on pushes
to `main` based on [Conventional Commits](https://www.conventionalcommits.org/):

| Commit prefix | Bump | Example |
|---------------|------|---------|
| `feat:` | **minor** (2.0.0 → 2.1.0) | `feat: add cost reporter workflow` |
| `fix:`, `perf:`, `refactor:` | **patch** (2.1.0 → 2.1.1) | `fix: dispatcher skips empty repos` |
| `feat!:` or `BREAKING CHANGE` | **major** (2.1.1 → 3.0.0) | `feat!: rename sfl.json schema` |
| `docs:`, `chore:`, `ci:`, `test:` | *no bump* | `docs: update README` |

When enabled, the workflow updates `VERSION`, stamps `sfl.json` and the HemSoft
version in `deployment/release-metadata.json`, creates a git tag (`v2.1.0`),
and publishes a GitHub release. Leave the variable unset until
the release metadata, artifact checksums, tag protection, prerelease, and
clean-machine installation checks in [`TODO.md`](TODO.md) are complete.

### How it works

| Repo | Version source | Badge shows |
|------|---------------|-------------|
| **This repo** (SFL source) | `VERSION` file | Latest released version |
| **Consumer repo** | `sfl.json` manifest | Version deployed to that consumer |

When you deploy to a consumer repo, the deploy script reads `VERSION`, stamps it into the consumer's `sfl.json`, and SHA-pins the source. To check if a consumer is current:

```
SFL VERSION file:  2.1.0    ← latest
Consumer sfl.json: 2.0.0    ← deployed version (behind)
```

### Adding the badge to a consumer repo

After deploying, add this to the consumer's README:

```markdown
[![SFL](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fraw.githubusercontent.com%2F<ORG>%2F<REPO>%2Fmain%2Fsfl.json&query=%24.version&label=SFL&color=FFD700&style=flat-square)](https://github.com/HemSoft/set-it-free-loop)
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
