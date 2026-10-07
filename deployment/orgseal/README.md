# Selected pilot App credential delivery

This preparation implements a separate credential delivery step for issue139.
The reviewed recipient policy allows only organization `hemsoft-dev`, ID338855369,
and private wider-workflow pilot1408025382. Reviewer-only repositories receive
no App key. Existing encrypted source and hs-buddy repository secrets retain
their overrides. This implementation does not distribute model credentials.

The manual source-main workflow authenticates the existing SFL App key with GET
requests, verifies registered and installation permission ceilings and explicit
unsuspended all-repository installation state, and seals the key in the runner.
Its artifact contains libsodium-compatible ciphertext and allowlisted public
metadata, retained for one day. The organization public key, key ID, decoded-key
SHA256 and selected repository ID are committed policy. Dispatch accepts no
recipient or public-key override. The helper validates policy and owner/main/run
context before obtaining the credential, accepts RSA private PEM only, and writes
ciphertext to a new mode0600 file. No owner token enters Actions.

Build and test with the declared Go1.25 toolchain:

```bash
cd deployment/orgseal
go mod verify
go vet ./...
go test -mod=readonly ./...
```

The tests use generated synthetic RSA and Curve25519 keys. An independent system
libsodium verifier decrypts the generated ciphertext and rejects tampering and
wrong recipients. Install `libsodium23` for the interoperability test. The pinned
Go crypto module is v0.55.0, which supports the repository's Go1.25 toolchain.

After this preparation is reviewed and merged, obtain owner approval for the
credential delivery. Before dispatch, confirm that the current organization
public-key GET still matches the committed key and the target remains the exact
private pilot. A changed recipient key requires a reviewed policy update.
Dispatch the sealing workflow only from reviewed source main. Keep the source
and App ownership window governed by all mandatory issue138 gates.

The local delivery helper uses the existing authenticated HemSoft CLI credential.
It downloads the ciphertext artifact in memory and validates the successful
owner-dispatched main run, current source head, executed public policy, artifact
server digest, exact run attempt, App identity, recipient and observation times.
It rechecks the organization ID/public key, active owner membership and private
pilot metadata. It refuses an existing organization App secret rather than
replacing it silently. Existing App variables must match the exact private-pilot
selection and values.

Preparation is read-only:

```bash
python3 -B deployment/scripts/deliver-sfl-org-credential.py \
  --source HemSoft/set-it-free-loop --reviewed-sha REVIEWED_MAIN_SHA --run-id RUN_ID
```

Only after explicit owner approval, add `--apply` to that same reviewed command.
This creates the public `SFL_APP_ID` and `SFL_APP_CLIENT_ID` organization variables
and writes `SFL_APP_PRIVATE_KEY` ciphertext with `visibility=selected` and the
sole selected repository ID1408025382. It repeats live preflight immediately
before writing and verifies resulting variable and secret selections. The output
receipt contains public identities/digests and no ciphertext or private key.

If a write fails after a public variable is created, keep the partial state
visible and inspect its public selection before retrying. If the secret exists,
the helper stops for a separately reviewed replacement decision. It never deletes
an existing secret or broadens selection. Follow delivery with the pilot's actual
App authentication and configured deterministic Auditor/Dispatcher proof.
Authentication metadata alone does not prove the wider runtime.

All tests and implementation preparation can run without accessing the actual
App key, writing organization credentials, invoking a model, or changing a
database. The migration issues remain open until their live proof completes.
