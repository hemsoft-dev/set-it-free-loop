# Scoped GitHub publication HTTP

`GitHubPublicationHTTP` implements the publisher's HTTP write and reconciliation
boundary. It is an explicitly constructed library, exercised only with fake
GitHub servers. It does not read credentials, create an App, poll, select a
provider or activate the retired controller.

Construction requires exact alpha enrollment, a trusted native-history reader
and an explicit publisher HTTP transport. The caller must separately attest
that transport's credential as the enrolled selected-repository installation,
with only the required repository permissions. Passing integers to construction
is not proof of token identity, installation selection or native picker support.
No production credential factory is supplied. Models never receive this object.

The adapter fixes the API origin and canonical repository path. Every public
operation rejects wrong repository/installation/PR scope before HTTP or delegated
reads. HTTP publication/reconciliation additionally verifies the canonical
repository's immutable ID. No model-controlled URL, redirect, cookie jar or
default environment proxy is selected by the adapter. The explicit transport
remains part of the trusted publisher boundary.

## Writes and receipts

Review writes use only COMMENT, the captured commit and right-side line anchors.
Configuration acknowledgments use issue comments. Both carry the durable unique
intent marker. The adapter validates marker/hash, finite document bounds and
scope before writing. It never approves, requests changes or merges.

A successful review response is insufficient alone. The adapter retrieves every
inline comment page and reconstructs the actual document. It checks the immutable
App author, review/PR identity, COMMENTED state, submitted time, actual commit,
original inline commit/line/side, and every inline body. Canonical path/line/body
ordering makes the hash independent of remote comment creation order. Duplicate
IDs, replies mixed into the original review, changed/missing comments, wrong
commits and ambiguous markers cannot confirm an intent. The publisher now checks
the receipt's actual commit independently of the summary/document hash.

Reconciliation exhausts review or acknowledgment pages, then each candidate
review's inline pages. Collection IDs cannot repeat. A next link must remain on
the fixed origin and exact collection, with the next numeric page and fixed page
size. Malformed, looping, foreign and duplicate-query links fail the scan. Only
complete scans return complete=true; an empty complete scan still does not
authorize a duplicate POST in the durable publisher.

Each HTTP request has a ten-second timeout, a two-MiB response cap and strict
JSON duplicate-key/trailing-data rejection. Public operations have a thirty-second
context; pagination has a finite one-hundred-page cap. Writes have no replayable
body, idempotency header, redirect or application retry. Errors expose status
and bounded Retry-After seconds, never submitted content or GitHub error bodies.
A timeout, lost response, malformed receipt or partial scan retains the durable
publication_unknown boundary. It does not prove that the remote write failed.

Supersession reads and verifies the exact known App review, then PUTs its summary
with a fixed marker and sanitized reason. It never appends another review or
comment. Repeating the same annotation is a no-op, including recovery after an
accepted update whose local response was lost. A conflicting annotation defers
for reconciliation.

New intents persist canonical comment order. Existing deployment state would
need an explicit migration before adopting a different document-hash policy;
this frozen alpha has no live installation/state migration path.

## Offline validation and remaining work

Tests send actual TLS HTTP requests through an explicit local fake transport.
They exercise COMMENT and ACK submission, multi-page inline receipts, altered
remote bodies/anchors/identity, reordered comments, redirect refusal, bounded
malformed responses and supersession idempotence. An integration test combines
SQLite, Publisher and HTTP: a server accepts a review then returns 503, delayed
visibility keeps publication unknown, a second Dispatch makes no POST, and a
later complete scan confirms exactly the original write.

Native request/history HTTP collection, selected-installation credential
attestation and private polling/runtime packaging remain [issue #176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176) work. The
injected read interface does not infer an owner request from a pending username.
Actual App picker, token scope, GitHub rendering and non-atomic publication race
qualification remain separate live gates. Owner-disabled Actions and all
activation restrictions remain unchanged.

Protocol sources checked October 20, 2026: GitHub's [review API](https://docs.github.com/en/rest/pulls/reviews)
defines COMMENT, commit_id, inline review comments and PUT summary updates.
Its [issue comment API](https://docs.github.com/en/rest/issues/comments) defines
ACK creation/readback. The [review comment API](https://docs.github.com/en/rest/pulls/comments)
documents original anchors and commit provenance. Tests use these documented
shapes with synthetic IDs/data, not captured live App evidence.
