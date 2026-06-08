<!-- lean-ctx -->
## lean-ctx

Prefer lean-ctx MCP tools over native equivalents for token savings.
Full rules: `C:\Users\User\.codex\LEAN-CTX.md`
<!-- /lean-ctx -->

## Lean-ctx Memory Protocol

Use lean-ctx memory for every substantive session, regardless of starting
folder.

At session start:
- Load continuity with `ctx_session(action: "load")` or inspect status with
  `ctx_session(action: "status")`.
- Recall relevant folder/project knowledge with
  `ctx_knowledge(action: "recall", query: "<task/repo/topic>")`.
- Use `ctx_overview(path: "<cwd>", task: "<task>")` for non-trivial repo work.

During work:
- Record resumable state with `ctx_session(action: "task"|"finding"|"decision")`.
- Keep `ctx_session` entries concise and tied to the active folder/session.

At closeout:
- Do a brief private self-reflection before the final response.
- Save current continuity with `ctx_session(action: "save")` when meaningful
  findings, decisions, or unfinished task state exist.
- Promote stable, verified folder/repo facts with
  `ctx_knowledge(action: "remember")` or reusable patterns with
  `ctx_knowledge(action: "pattern")`.
- Skip one-off command output, secrets, speculation, and transient paths.

Scope rules:
- Folder-specific knowledge belongs in lean-ctx for the current project/root.
- Cross-repo or machine-wide conventions must be explicitly labeled as global
  in the key/value, for example `global_codex_memory_closeout_policy`.
- Do not store repo-specific facts against a drive root like `D:/` unless the
  task is genuinely about that root.
- Prefer starting Codex in the real repo/folder root. If started too high,
  reroot mentally and avoid polluting broad roots with narrow project facts.

Do not mention the reflection unless it changes the user-facing outcome.

## Repository Identity

This checkout is the private HemSoft copy of `HemSoft/set-it-free-loop`.

When using GitHub CLI or GitHub MCP/API tools for this repository, use the
`HemSoft` GitHub account.

When using Git remotes for this repository, use the `github-personal1` SSH
profile. This profile authenticates to GitHub as `HemSoft`. The expected remote
URL is:

```text
git@github-personal1:HemSoft/set-it-free-loop.git
```

Do not use `github-work1` for this repository. That profile authenticates as
`fhemmerrelias` and only has read access to the private HemSoft repositories.
