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

## graphify

This project has a knowledge graph at graphify-out/ with god nodes, community structure, and cross-file relationships.

When the user types `/graphify`, use the installed graphify skill or instructions before doing anything else.

Rules:
- For codebase questions, first run `graphify query "<question>"` when graphify-out/graph.json exists. Use `graphify path "<A>" "<B>"` for relationships and `graphify explain "<concept>"` for focused concepts. These return a scoped subgraph, usually much smaller than GRAPH_REPORT.md or raw grep output.
- Dirty graphify-out/ files are expected after hooks or incremental updates; dirty graph files are not a reason to skip graphify. Only skip graphify if the task is about stale or incorrect graph output, or the user explicitly says not to use it.
- If graphify-out/wiki/index.md exists, use it for broad navigation instead of raw source browsing.
- Read graphify-out/GRAPH_REPORT.md only for broad architecture review or when query/path/explain do not surface enough context.
- After modifying code, run `graphify update .` to keep the graph current (AST-only, no API cost).
