package reviewer

import (
	"context"
	"time"
)

// nativeVMResultGraceMax is the host's fixed wait for the guest's finished
// record at the end of a native session. It is not a tunable: both sides derive
// it from the captured phase deadline, Session.Limits.Timeout, which the host
// already sends to the guest in the bound launch request.
//
// The session ceiling stays the phase deadline. Within it, the last grace is
// reserved for returning auth and the source audit:
//
//   - The host ends client work at its own deadline minus the grace: it revokes
//     provider intake, exactly as a shutdown drain does, and exports no auth
//     after that point. It then reads the finished record until its unchanged
//     deadline, after which the existing fenced path runs.
//   - The guest ends its client, code host and broker at the phase deadline
//     minus the grace, counted from its own request intake, then stops its roles
//     and returns auth and the source audit in its ordinary finished record.
//
// The guest's intake follows the host's start by the host's preparation and VM
// boot, measured at a few seconds. A hung client therefore returns within the
// grace when boot plus the guest's cleanup after its client ends (normally well
// under a second, at most seventeen) stays under fifteen seconds. A guest that
// does not return in time is fenced exactly as before.
const nativeVMResultGraceMax = 15 * time.Second

// NativeVMResultGrace is the result grace for a phase deadline: fifteen seconds,
// or a quarter of a shorter deadline.
func NativeVMResultGrace(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	return min(nativeVMResultGraceMax, timeout/4)
}

// nativeVMGuestTimeout is the guest's own phase deadline, counted from its
// request intake.
func nativeVMGuestTimeout(timeout time.Duration) time.Duration {
	return timeout - NativeVMResultGrace(timeout)
}

// nativeVMWorkDeadline is when the host ends client work: the grace before the
// session context's actual deadline. A caller deadline earlier than the phase
// deadline moves it earlier too, so the caller always bounds the wait.
func nativeVMWorkDeadline(ctx context.Context, timeout time.Duration) (time.Time, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Time{}, false
	}
	return deadline.Add(-NativeVMResultGrace(timeout)), true
}
