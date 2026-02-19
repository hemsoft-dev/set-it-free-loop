# Set it Free Loop™

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

### 2. Deploy a workflow

```powershell
.\deployment\scripts\deploy-workflow.ps1 -Workflow weekly-repo-audit -Repos "org/repo1,org/repo2"
```

### 3. Review the catalog

See [CATALOG.md](CATALOG.md) for all available workflows, their triggers, and expected outputs.

---

## Workflow lifecycle

```
TODO.md (idea)
    ↓  /new-workflow prompt in VS Code
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
- [TODO.md](TODO.md) — ideas pipeline
- [VISION.md](VISION.md) — strategic vision
- [SOLVING-SOFTWARE-ENGINEERING.md](SOLVING-SOFTWARE-ENGINEERING.md) — operating model playbook
- [deployment/governance/policy.md](deployment/governance/policy.md) — governance policy
- [docs/intake-format.md](docs/intake-format.md) — sfl: frontmatter spec (maintainers)
