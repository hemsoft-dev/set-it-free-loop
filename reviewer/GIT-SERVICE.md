# Git reviewer service composition

The private Linux service owner joins the concrete Git source factory,
credential owner, polling controller and scoped GitHub publisher. This is an
internal assembly for the [public trusted startup API](GIT-STARTUP.md).
`OpenGitReviewerService` supplies measured release packages and retains the
outer startup resources around this owner. Neither layer installs a daemon,
imports credentials or grants activation authority. The existing owner gates
still apply.

## Startup and ownership

Inputs come from the trusted release layer. They include the durable controller
journal, already opened unit/archive/runtime owners, helper and fixed manifest,
qualified release/hash, source work directory, explicit transports, eligibility
checks and trigger policy. Preserve the trigger policy's original `EnabledAt`
across restarts; resetting it would change which events polling can discover.

Construction opens an unqualified credential owner and completes the concrete
[source factory's recovery](GIT-FACTORY.md) before assembling the controller.
It performs no signing, token mint, GitHub request, model invocation or polling.
The controller uses comment-command and automatic-event discovery, with the
same discovery reader supplying publication provenance. Reader and publisher
HTTP access use separate roles of the same credential owner. Raw capture and
restore callbacks cannot replace the source factory in this composition.

A nil service with an error means source ownership did not transfer. The
caller retains its unit, archive, runtime, helper and journal. The newly opened
unqualified credential owner is disposed. If that disposal fails, the returned
service is retained instead. A nonnil service with any error must be kept for
explicit `Recover` or `Close`. Once the source factory returns a nonnil handle,
it owns the unit, archive and runtime. The helper, transports and controller
store/journal remain caller-owned.

`Recover` is local and never refreshes credentials. Unknown old units or
unreturned work keep admission fenced and retain the original owners. Recovery
during shutdown may complete old physical-stop evidence but cannot restart the
controller or reopen admission.

## Explicit qualification and work

Only `Refresh` signs or mints. It holds the source factory's operation token
through readiness, root validation and credential qualification, so a new
capture cannot introduce an uncertain acquisition between those checks and
signing. Credential generation shutdown remains independent of that token.
Eligibility checks are repeated by credential and runtime operations. Checks
are trusted, read-only callbacks, must honor cancellation, and must not acquire
the service/factory operation token recursively.

`Run` requires a successful explicit refresh. It never refreshes automatically
and starts its controller at most once. Cancellation or a returned run error
does not dispose the source and credential owners. The caller must close the
service and retain it if that close cannot prove completion.

The concrete native runtime takes a bounded, copied catalog of explicitly
qualified releases and providers. A durable job selects its original release,
configured providers and identities. There is no default provider or fallback
release. Each provider gets its own reader bound to that job's admitted
snapshot and packet. The session gate binds job ID, snapshot, packet, config
hash, configured identity and round, then rechecks release and provider
eligibility. Native asset verification and containment still precede auth
acquisition. Catalog construction performs structural checks only.

An explicit alternate runtime callback supports fake tests and trusted alternate
assemblies. Supplying both that callback and the native catalog is rejected.

## Shutdown

`Close` first closes admission and cancels any active lifecycle phase and run.
It waits for the run to return, closes the controller, closes the concrete source
factory, then closes the credential owner. A failed earlier step retains later
owners. Its outer deadline is 30 seconds or the caller's earlier deadline.
Refresh and recovery have three-minute ceilings. A callback that ignores its
cancelled context remains owned; a timed-out close does not free its locks or
claim it stopped. Retrying `Close` operates on the same retained owners.

## Evidence and remaining gates

Service tests use measured inert acquisition assets, fake unit/kernel evidence,
synthetic credentials and local TLS GitHub fixtures. They cover startup without
signing, both HTTP roles, constructor ownership, uncertain recovery, cancelled
qualification, generation drain, native release selection and a model-free
polling run with both caller and service shutdown. Native session tests use
closed gates before unopened asset/auth paths. No model is invoked.

This assembly does not qualify a deployment. The public trusted operator entry
point and canonical release package are described in [GIT-STARTUP.md](GIT-STARTUP.md).
Trusted release promotion/rollback, native control tasks and the durable
evaluation runner remain source work. Complete host containment qualification,
provider policy/entitlement and quota evidence, final App/trigger evidence and
explicit owner activation remain separate gates tracked in
[issue #176](https://github.com/hemsoft-dev/set-it-free-loop/issues/176).
Existing failed physical containment qualifications remain failed.
