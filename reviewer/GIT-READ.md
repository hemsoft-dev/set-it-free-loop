# Scoped Git read exchange

`NewGitHubGitReader` accepts an already qualified reader installation transport. The resulting opaque capability shares its private credential state. Publisher credentials, zero handles and closed credentials are refused. Neither construction nor exchange acquires, recovers or renews a credential.

The typed request represents only advertisement or upload-pack and protocol version 0 or 2. It has no caller URL, repository, header or credential field. Exchange creates exactly one fresh HTTPS request to one of these routes:

| Operation | Fixed route |
| --- | --- |
| Advertisement | `GET https://github.com/hemsoft-dev/set-it-free-loop.git/info/refs?service=git-upload-pack` |
| Upload-pack | `POST https://github.com/hemsoft-dev/set-it-free-loop.git/git-upload-pack` |

GitHub documents installation-token HTTP Git access with Contents permission and the token as HTTP password. This component supplies the private reader token with the fixed `x-access-token` username. The normal REST transport still refuses the Git origin and reader POST requests. The separate capability grants no receive-pack or publisher operation. [GitHub installation authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)

Requests preserve the supplied binary bytes and set the fixed Git content types. Version 2 adds `Git-Protocol: version=2`; version 0 omits it. The complete operation has a 90-second ceiling, including the returned body, and honors an earlier caller deadline. A caller trace is refused before dispatch. POST bodies are nonreplayable and carry no caller-controlled headers. The trusted underlying RoundTripper must honor contexts and must not hide redirects, credential fallback or retries. [Smart HTTP routes](https://git-scm.com/docs/gitprotocol-http), [version 2 negotiation](https://git-scm.com/docs/gitprotocol-v2)

The request body limit is 4 MiB. Advertisement responses are limited to 4 MiB and upload-pack responses to 64 MiB. Empty POSTs and excess requests are refused before HTTP. Known excess response lengths are refused before returning a body; unknown lengths are checked while streaming, with no byte past the limit delivered. A one-byte probe distinguishes an exactly full body from overflow. EOF, an error, explicit Close or context cancellation closes the underlying response. Request bodies must allow concurrent Close to unblock Read; trusted transports and their bodies must honor cancellation and bounded Close. An uncooperative injected implementation cannot be made interruptible by this library.

Only a bounded body returns. The response object, upstream headers, cookies, auth challenges, redirects and non-200 error bodies remain private. Accepted responses require the exact expected content type and an identity representation. Automatic decompression and compressed responses are refused. The [local broker](GIT-BROKER.md) separately bounds compressed and decoded request bytes before passing decoded bytes here. A successful exchange transports bytes; it does not validate a Git pack, establish object completeness or qualify any source store.

The capability checks the reader's qualification time and the existing one-minute expiry margin before dispatch. Credential Close makes existing copies refuse new work, but does not cancel an exchange already admitted. An outer generation owner must keep a lease through its call and returned body, then drain or cancel those leases before disposal. The separate Unix broker has its own protocol tests. Protected peer policy, aggregate acquisition resources, fresh quarantine, exact full-history verification and final owner integration still require production composition. The credential-free source reader keeps its no-fetch behavior.

## Validation

The private `gitGenerationReader` now connects this exchange to a pinned
acquisition-generation borrow. The broker receives only the typed exchanger,
with no owner, refresh operation, publisher handle or credential. Admission
failure closes the input body. Exchange failure retires only its HTTP borrow;
the whole-acquisition lease remains. Successful exchange holds that borrow
through response closure and every admitted Read returning. Generation
cancellation closes the response, but an unreturned Read still blocks borrow
retirement. The factory must separately revoke and join the broker and prove
physical unit stop before releasing the whole generation. This private wiring
does not create the remaining production factory or activate a source service.

Its focused tests use actual local TLS for both protocols and both operations,
verify pinned token use and body retirement, and independently reject a live
inert process as stopped evidence. Another response deliberately returns from
Close while Read remains blocked. Cancellation closes it exactly once, but the
owner retains its HTTP borrow and token pair until Read returns and the actual
inert process is stopped. Failure tests assert no extra dispatch, closed input,
scrubbed errors and retained whole-generation ownership.

The wrapper preserves the typed exchange's scrubbed cancellation/deadline
cause. Its fixed 90-second operation ceiling can expire while the outer
acquisition remains live; that still matches `context.DeadlineExceeded`.
`SFL_GIT_FIXED_TIMEOUT=1` enables the separate actual 90-second integration
test. It is skipped by default to avoid repeating that wait in every full
suite; its explicit before/after execution is recorded separately. The test
uses the production timeout and synthetic transport, never a shortened bound.

Tests run the installed Git HTTP client and `git-http-backend` against a synthetic repository. A loopback test bridge selects the typed operation, which authenticates to a real local TLS upstream. The fixture verifies the upstream Basic identity, strips it, and invokes the actual Git backend without the token. Actual protocol v0 and v2 fetches each exercise advertisement and upload-pack, pass full strict fsck and compare every fetched object's raw bytes with its source. Binary blobs and unusual paths are included. The test bridge is not production peer isolation.

Other tests cover protocol/media headers, exact binary request and response preservation, role/operation/context refusal, unchanged REST restrictions, Close and expiry refusal, malformed/ambiguous headers, redirects, auth failures, compressed responses, known and streamed excess bodies, exactly full POST responses, private transport/read/close errors, caller body ownership and cancellation during input and output. Handles and returned bodies redact value, pointer and JSON formatting. Only synthetic credentials and local transports are used. Installed executable and backend provenance are recorded with the mission receipts; the host-specific producer test skips when its backend is unavailable.

This source does not activate the retired application, install an App, discover or import credentials, invoke a provider, spend funds or establish production GitHub access. Related controller and live acceptance criteria remain open in #176 and its dependent issues.
