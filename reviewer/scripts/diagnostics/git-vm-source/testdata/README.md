# Actual guest and host records

`actual-guest-result.b64` losslessly encodes the complete version 2 result line
from the credential-free VM probe on October 21, 2026 at 11:07 PM EDT. The
paired `actual-host-receipt.json` retains the same nonce, original manager
invocation, cgroup identity and observed limits, final stopped state, runtime
verification and source/program/image pins. The original cgroup was removed.
No success field was supplied by a hand-built guest fixture.

The producer checkout was `a71b39e48e42b8114685989f58b1cef60cef63cb`.
Its C source SHA-256 is
`1682e71ce49d521db912c99ac7aa8244d16e52c123f57d26ba241cfaa4e7ff33`.
The decoded console line SHA-256 is
`21c390dfc7825f364208343bb785843e299c8126a391a60897edccf240715587`.
Nonce `013821658b5b49d4845db4613b935413` binds that one run. The kernel
reported `7.0.0-38-generic`. Retained source hashes distinguish this capture
from later commits that add the records, tests or documentation.

The host used private verified copies of the measured QEMU program, captured
libraries and ROMs, kernel, firmware and newly built inert initrd. The original
root-owned namespace helper bound that runtime read-only. Its own host
interpreter and libraries remain explicit trusted bootstrap prerequisites.
The receipt records only inert prerequisite and shutdown observations. It is
not production acquisition, aggregate confinement, provider or model-quality
qualification. `production_qualified` remains false.

`legacy-guest-result-v1.b64` preserves the earlier actual producer record from
October 21, nonce `883c661671d20c825627c9d854192bae`, with decoded SHA-256
`42bd090440c0adbb8db6e74d3bb347b385dca8dde19103a3214983dd8c215085`.
That earlier diagnostic source had SHA-256
`32535a29ad3bcc9227ca53cc1633e8c7eac9404422fb0ea9227bb95353c33902`.
It was separate from its host-helper checkout `917d854`, not claimed to be
present there. Version 1 lacks work-mode, owner and actual write evidence and
is now rejected. It supplies no host-shutdown proof. Preserve its original
CRLF bytes rather than upgrading it into a new observation.

Mutated records exercise refusals. They are not additional observed runs.
