# Provider support send pack, October 27, 2026

Status: ready for Franz to send, unsent. This pack implements the
[October 27 owner approval](https://github.com/hemsoft-dev/set-it-free-loop/issues/173#issuecomment-6448028772)
and preserves the [prepared questions](2026-10-21-alpha-activation-checklist.md#updated-provider-questions-prepared-only).
The implementation worker did not contact either provider, sign in, inspect
account credentials, change billing or activate SFL.

## Who sends it and where

Franz sends each message through his own subscription account. The account-bound
support route needs Franz's hands under this assignment's no-login instruction.
He supplies his current subscription tier and account email privately in the
support conversation. Those values are deliberately absent from this public pack.

- OpenAI Product Support, for Codex subscription workflow and policy questions.
  Open the support messenger at the [OpenAI Help Center](https://help.openai.com/)
  and paste the OpenAI message below. Request a written, account-specific answer
  or routing to the team responsible for Codex subscription policy. The
  [official contact instructions](https://help.openai.com/en/articles/6614161-how-can-i-contact-support)
  identify that messenger as the contact route.
- Anthropic Product Support, for Claude Code subscription workflow and policy
  questions. From Franz's existing [Claude account](https://claude.ai/), select
  his name, then Get help, then Send us a message. Paste the Anthropic message
  below and request Product Support escalation if needed. The
  [official support instructions](https://support.claude.com/en/articles/9015913-how-to-get-support)
  document this route and replies in the same conversation and account email.

Both contact instructions were fetched on October 27, 2026. Reuse an existing
support conversation for this question if one already exists. No contact has
been made by this worker, and no support case ID exists in this tranche.

## OpenAI message

Subject: Written confirmation for owner-only unattended public-PR review using Codex subscription auth

Hello OpenAI Product Support,

I own the public GitHub repository
https://github.com/hemsoft-dev/set-it-free-loop, repository ID 1169772257.
Before enabling a private review service for my own use, I need written
confirmation that this exact workflow is supported by my subscription.

The proposed service runs the pinned, unmodified official Codex CLI 0.163.0
on my isolated private Linux host using my own ChatGPT subscription login.
It reviews PRs only in that repository, starting from my authorized
`@sfl review` comments or my enrolled automatic PR-open/head-push polling policy.
It includes initial review, peer critique with another provider, and bounded
comparison runs. Reviews would be posted on my public repository. I am the
only account user; there is no shared credential, customer service, account
resale, API-billing fallback or assumption that a private host changes policy.
The proposed model pin is `gpt-6-astra`; access has not been established for
this worker profile. The service is not activated.

1. Is this exact unattended, owner-only public-repository workflow permitted
   and supported with my subscription and the unmodified official CLI? Please
   cite the applicable policy or provide a scoped written confirmation.
2. Our project recorded a public/open-source exclusion for cached-account CI
   in https://learn.chatgpt.com/docs/auth/ci-cd-auth. Does that exclusion apply
   to this private-host service? If guidance has changed, please identify the
   current applicable rule. We will not infer an exception from a successful
   request or from separate API-key CI guidance.
3. Which supported subscription authentication mechanism may an unattended
   worker use, including separate storage, expiry, refresh, serialization and
   reauthentication? Is a separate owner-only worker profile allowed, without
   sharing my interactive profile with jobs? We will not modify the client or
   distribute subscription credentials to another person.
4. Which subscription entitlement and model-access evidence must I retain for
   this account, client version and model? If the pinned version or model is
   unsupported, please say so rather than assuming an upgrade or substitution.
5. How can I enforce subscription-only usage with no purchased-credit draw,
   automatic top-up, API billing or other paid overflow? Please identify the
   account settings and worker-side checks that make it stop at quota and
   fail closed if the auth or billing mode changes.

Please distinguish workflow permission from account entitlement, supported auth
and paid-usage controls. If you cannot confirm a part, please mark it unconfirmed
and route it to the appropriate team. Please do not change my plan, enable
credits, create credentials or activate anything as part of this inquiry.

Thank you,
Franz

## Anthropic message

Subject: Written confirmation for owner-only unattended public-PR review using Claude Code subscription auth

Hello Anthropic Product Support,

I own the public GitHub repository
https://github.com/hemsoft-dev/set-it-free-loop, repository ID 1169772257.
Before enabling a private review service for my own use, I need written
confirmation that this exact workflow is supported by my individual subscription.

The proposed service runs the pinned, unmodified official Claude Code CLI
2.1.303 on my isolated private Linux host with my own Claude subscription.
It reviews PRs only in that repository, starting from my authorized
`@sfl review` comments or my enrolled automatic PR-open/head-push polling policy.
It includes initial review, peer critique with another provider, and bounded
comparison runs. Reviews would be posted on my public repository. I am the
only account user; there is no shared credential, customer service, account
resale or API-billing fallback. The proposed model pin is `claude-opus-5-5`;
access has not been established for this worker profile. The service is not
activated.

1. Is this exact unattended, owner-only public-repository workflow permitted
   and supported under the individual-use and credential-use terms for my
   subscription? Please cite applicable policy or provide scoped written
   confirmation, including how ordinary individual usage applies here.
2. Which supported subscription credential mechanism may this unattended
   official-client worker use? Please cover separate worker storage, expiry,
   refresh and reauthentication without sharing my interactive profile with
   disposable jobs or distributing credentials to another person.
3. Which account-specific subscription and model-access evidence must I retain
   for this client and `claude-opus-5-5`? If the pinned version or model is
   unsupported, please say so rather than assuming an upgrade or substitution.
4. How can I enforce subscription-only usage with extra usage, usage credits,
   automatic top-up, API billing and other paid overflow disabled or blocked?
   Which account settings and worker-side checks make quota exhaustion and
   changed auth or billing modes stop work without extra charges?

Please distinguish workflow permission from account entitlement, supported auth
and paid-usage controls. If you cannot confirm a part, please mark it unconfirmed
and route it to the appropriate team. Please do not change my plan, enable
extra usage, create credentials or activate anything as part of this inquiry.

Thank you,
Franz

## Evidence to bring back

Keep each full response privately with its case ID, date and timezone, provider
identity, account and plan scope, exact submitted message, and applicable policy
links. Add only a sanitized summary or scoped written confirmation to
[#173](https://github.com/hemsoft-dev/set-it-free-loop/issues/173). Never attach
auth files, tokens, full transcripts, account emails or billing details to the
public issue. A generic bot answer, successful request or permission for a
different workflow does not establish this workflow's eligibility.

Provider confirmation alone does not close #173. Account entitlement, separate
worker auth and refresh, enforceable paid-overflow prevention, remaining
containment and evaluation gates, the hostile-metadata/code-mode block and a
separate explicit live-activation go still apply. The October 27 approval permits
owner-only subscription use once those gates pass; it does not itself activate
the service. [#264](https://github.com/hemsoft-dev/set-it-free-loop/issues/264)
remains human-required. Work on
[#176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176) and
[#177](https://github.com/hemsoft-dev/set-it-free-loop/issues/177) follows
[#174](https://github.com/hemsoft-dev/set-it-free-loop/issues/174).

Council: active GPT 6.1 Sol · consulted: none · missing: Claude Opus 5.5,
over pace; Muse Wonder Full 1.0, quota guard unknown · adopted: 0 · declined: 0.
Council unavailable. The live `/v1/capacity` report was read through Chief
`scripts/usage.py --json` on October 27. Claude weekly projected 143% against
SUS 70, and both Muse windows were unknown. No seat was forced. This pack
changes no activation design or gate.
