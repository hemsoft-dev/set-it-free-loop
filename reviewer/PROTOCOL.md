# Adversarial protocol and per-PR controls

This implements the offline contract for [issue #175](https://github.com/hemsoft-dev/set-it-free-loop/issues/175). The protocol component does not launch inference or publish reviews. The module separately supplies [native VM](NATIVE-VM.md) and [controller/publication](CONTROLLER.md) composition. Their live qualification and activation gates remain required.

`DefaultConfig` selects both pinned providers in adversarial mode, four maximum reviewer sessions, sequential execution, four reviews per day and a 24-hour maximum quota wait. Trusted defaults may tighten resource bounds. Explicit modes are adversarial, independent-pair, codex-only and claude-only. Independent-pair has two R1 sessions; a single-provider selection has one. Those selected modes complete successfully with unverified summary findings, never peer-supported inline allegations.

## Owner commands

```text
@sfl review
/sfl config mode=adversarial
/sfl config mode=adversarial rounds=2
/sfl config mode=claude-only model.claude=default
/sfl config show
/sfl config reset
/sfl cancel
```

Only original, unedited commands whose trusted GitHub author metadata names immutable owner ID `8227352` are accepted. Unknown keys, duplicate assignments, unlisted models, arbitrary flags/endpoints and excessive rounds fail. Non-owner commands are ignored. The parser and configuration functions perform no inference or writes. Reset restores the controller's trusted defaults. Selecting a mode without peer rounds resets the round setting to one unless the command explicitly specifies it.

The durable controller owns command acceptance, acknowledgment intent/marker reconciliation and request ordering. A configuration change affects requests created after its acknowledged version; it does not alter captured running/resumable jobs or trigger review. Cancel fences the generation; replacement work needs a fresh authorized `@sfl review` comment or a subsequent automatic head observation. The selected polling-trigger contract is documented in [GITHUB-TRIGGERS.md](GITHUB-TRIGGERS.md). Source files, PR bodies and model output are never command metadata. An accepted command's durable decision cannot be rewritten by edits or deletion.

## Bounded execution

`Job` captures owner/request generation, exact snapshot, typed config/version, packet and trusted release. `NewSession` supplies precomputed immutable hashes for the model to copy. R1 has no peer reports. Both R1 outputs must validate before R2; each R2 addresses every peer finding. An explicitly preselected extra paired round receives both R2 reports and R1 lineage, focuses only outstanding disputed/unverified claims, and requires each focused own claim to be answered or withdrawn. No extension after completion is offered.

The engine validates structured output, immutable references and source quotes. It preserves valid phase artifacts for same-job resume and counts consumed sessions across checkpoints. Only a trusted pre-work failure can refund that session count. An uncertain or invalid phase consumes budget; a retry cannot create a fifth default session. Quota/error outcomes never switch to a single provider. Incomplete output contains status only, with no substantive partial findings. A production driver must enforce external process/resource deadlines; cooperative Go contexts and fake tests are not host-isolation proof.

The deterministic ledger records each allegation, original provider/round, every peer judgment, response, withdrawal and related exact duplicates. Peer support is model-assessed. Rejection preserves a dispute; insufficient examination and new R2/R3 allegations remain unverified. R3 may promote a late finding only with explicit cited peer confirmation. Origin withdrawals stay retracted. Severity differences remain visible. No model adjudicator or forced consensus is used.

`Artifact.ValidationReads` records the engine's source-validation reads. It does not claim to contain the model's complete source-access audit; the separately qualified reader/broker must retain those actual access receipts. Authoritative omissions survive in reports and artifacts. Resumed phases preserve their prior coverage.

`Artifact.SourceAccess` separately retains the session-bound broker receipt from
source-capable adapters. Fake workers exercise actual broker reads; legacy
reviewers have a nil receipt rather than invented zero-read evidence. Durable
attempt history preserves broker reads on errors as well as successful phases.
See [SOURCE.md](SOURCE.md) for capability, budget, recovery and live-boundary limits.

## Rendering and validation

`Render` produces a bounded summary and inline candidates. It combines exact duplicate allegations while preserving their IDs and assessments, neutralizes mentions, removes external model links and escapes model HTML/Markdown. Only trusted snapshot source links are generated. Inline eligibility comes from a trusted diff-line map; other valid locations remain in the summary. Revisions, selected models, CLI/auth mappings, config and release provenance are recorded in generated metadata.

Model text cannot produce GitHub reference autolinks. `@` and `#` become fullwidth `＠` and `＃`, which removes mentions, `#N`, `owner/repo#N`, `owner@SHA` and `owner/repo@SHA`. `GH-N` in any case, unless glued to a preceding ASCII letter or digit, becomes `GH－N` with a fullwidth hyphen-minus. Each maximal run of 7 or more ASCII hexadecimal digits, in any case, that is not glued to an ASCII letter or digit on either side gets an invisible U+2060 word joiner after every 6 digits, so no piece can match a commit SHA. Runs glued to other letters or digits are left alone because GitHub's word-boundary match cannot select them either. HTML escaping uses named entities only, so escaping never reintroduces `#` followed by digits. The same rules apply inside code spans. Trusted revisions and snapshot source links are not passed through this filter. Known-secret redaction tolerates the joiners and fullwidth hyphen-minus that this filter inserts depending on neighbouring text.

Run the declared gates from the repository root:

```sh
go -C reviewer vet ./...
go -C reviewer test -race -count=1 ./...
```

The reviewed Markdown fixture is synthetic and proves no App eligibility, live model quality or actual GitHub publication. To intentionally regenerate it after inspecting a format change:

```sh
go -C reviewer test -run TestReviewMarkdownGolden -args -update-golden
```
