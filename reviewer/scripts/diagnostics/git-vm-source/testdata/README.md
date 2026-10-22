# Actual guest producer record

`actual-guest-result.b64` losslessly encodes the complete result line from the credential-free
VM probe on October 21, 2026, retained with its original console line ending.
The private capture used the measured native image's kernel and firmware pins,
with repository snapshot `917d854a5b303aafe92960515bf98dd294710147` for the host
fixture helper. The probe itself was separate diagnostic source, not claimed to
be present in that earlier repository snapshot.

The committed `probe.c` has the same source SHA-256 as that producer,
`32535a29ad3bcc9227ca53cc1633e8c7eac9404422fb0ea9227bb95353c33902`.
Decode the single base64 line to recover the original CRLF console bytes.
The decoded result line SHA-256 is
`42bd090440c0adbb8db6e74d3bb347b385dca8dde19103a3214983dd8c215085`.
Nonce `883c661671d20c825627c9d854192bae` identifies that one historical run.
The kernel reported `7.0.0-38-generic`.

This is a positive artifact from the actual pinned producer. Mutated records
exercise refusal; they are not additional observations. The record establishes
only its reported inert prerequisites and always has `production_qualified`
false. It is not a source acquisition, provider, App or model-quality result.
