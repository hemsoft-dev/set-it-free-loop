//go:build linux

package reviewer

import "time"

// fakeWorkClock drives the host's work-deadline timer in tests. It reports a
// fixed now, captures the armed delay and callback, and fires only when the
// test calls fire.
type fakeWorkClock struct {
	now     time.Time
	delay   time.Duration
	fire    func()
	stopped int
}

func (c *fakeWorkClock) install(p *nativeVMHostPhase) {
	p.now = func() time.Time { return c.now }
	p.after = func(d time.Duration, f func()) func() bool {
		c.delay, c.fire = d, f
		return func() bool { c.stopped++; return true }
	}
}
