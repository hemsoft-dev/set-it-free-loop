# Native unit producer record

`actual-native-unit.json` is the native supervisor's actual nonsecret unit
journal from the first local synthetic process-loss run on October 21, 2026 EDT.
The driver source was `8d78dc5183f473d50c5f505381d89a8c62445168`, with native image
source `7dd5d612016b9ea0bd396230f0f5fc6342943d88`. It records one stopped
fake-only unit and includes no auth bytes, model output or real account data.

The initial consumer declared an untagged `InvocationID` field and silently lost
`invocation_id`. This producer record made its regression fail before the
consumer switched to the existing `GateIdentity` type and required the complete
captured identity. The first run is intermediate evidence, superseded for final
qualification by the corrected driver's separate fresh runs.

`actual-exported-auth-journal.json` is the nonsecret metadata journal retained
from the forced Codex driver-timeout run at source
`3315dbee3f5924e032ac2f3433b1ac2cbb5e5723`, with the same measured image source.
It contains only provider/binding, active/exported lifecycle and hashes. The
producer regression verifies that this evidence survives later synthetic
retirement. No credential or profile content is included.
