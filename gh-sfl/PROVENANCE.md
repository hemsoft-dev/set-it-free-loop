# Source provenance

The HemSoft `gh sfl` source started from the `gh-sfl/` tree in
`relias-engineering/set-it-free-loop` release `v6.5.7` at commit
`8d5e30714fa6cc61a89189266f8eb463132abde9`.

HemSoft imported that reviewed source on 2026-08-13. The repository now owns
its private-scope adaptations as ordinary Go source. Builds and tests must use
this directory and must not require a Relias checkout.

The intentional HemSoft changes include repository identity, deployment tiers,
workflow path routing, manifest engine metadata, private target authorization,
the `github-personal1` SSH transport, label policy, and engine configuration.
