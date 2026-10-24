package reviewer

import (
	"context"
	"sync"
	"time"
)

// providerShutdownGrace bounds how long PollingController.Close waits, once,
// for an in-flight provider session that has already exported auth. Close
// revokes that session's provider intake, so the guest's client ends and the
// guest can return auth and the host can stop the unit and write its stopped
// receipt. After the grace, or the caller's earlier deadline, Close cancels as
// before. The grace never resets: a second Close or a hanging guest gets none.
//
// Ten seconds covers the guest's own bounded cleanup after its client ends
// (role stop and auth read, at most twelve seconds, normally well under one)
// and the result record. Together with the native host's fifteen-second stop
// path after a late cancellation it stays inside GitReviewerService.Close's
// thirty-second ceiling. A session's own deadline is never extended.
const providerShutdownGrace = 10 * time.Second

// shutdownDrain is the private signal from PollingController.Close to its one
// active invocation, carried in that invocation's context. A provider session
// holds it before exporting auth; only a held drain earns the grace.
type shutdownDrain struct {
	mu     sync.Mutex
	held   int
	begun  bool
	signal context.Context
	fire   context.CancelFunc
}

func newShutdownDrain() *shutdownDrain {
	signal, fire := context.WithCancel(context.Background())
	return &shutdownDrain{signal: signal, fire: fire}
}

type shutdownDrainKey struct{}

func withShutdownDrain(ctx context.Context, d *shutdownDrain) context.Context {
	return context.WithValue(ctx, shutdownDrainKey{}, d)
}

// shutdownDrainOf returns the invocation's drain, or nil outside a controller
// invocation (evaluation runs, fixtures), which never get a grace.
func shutdownDrainOf(ctx context.Context) *shutdownDrain {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(shutdownDrainKey{}).(*shutdownDrain)
	return d
}

// hold registers a session about to export auth. It refuses once the drain
// has begun, so no auth is exported after Close started. release is
// idempotent.
func (d *shutdownDrain) hold() (release func(), ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.begun {
		return nil, false
	}
	d.held++
	return sync.OnceFunc(func() {
		d.mu.Lock()
		d.held--
		d.mu.Unlock()
	}), true
}

// begin starts the drain and fires its signal. It reports true only to the
// first caller and only while a session holds exported auth: that caller
// alone may wait the grace.
func (d *shutdownDrain) begin() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	first := !d.begun
	d.begun = true
	d.fire()
	return first && d.held > 0
}

func (d *shutdownDrain) draining() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.begun
}

// shutdownDraining reports whether Close has begun draining ctx's invocation.
// The engine starts no further phase once it has.
func shutdownDraining(ctx context.Context) bool { return shutdownDrainOf(ctx).draining() }
