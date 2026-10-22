//go:build linux

package reviewer

import (
	"context"
	"strings"
	"testing"
)

// This replacement happens after Observe returns the original identity, at
// the precise gap before the old StopUnit-by-name call.
type replacementDuringStopManager struct {
	*fakeContainmentManager
	armed, replaced, replacementStopped bool
}

func (m *replacementDuringStopManager) Observe(ctx context.Context, unit string) (unitObservation, error) {
	old, err := m.fakeContainmentManager.Observe(ctx, unit)
	if m.armed && !m.replaced && err == nil {
		m.replaced = true
		m.mu.Lock()
		m.observation.InvocationID = strings.Repeat("e", 32)
		m.mu.Unlock()
		m.kernel.mu.Lock()
		m.kernel.group.Inode++
		m.kernel.mu.Unlock()
	}
	return old, err
}
func (m *replacementDuringStopManager) Stop(ctx context.Context, unit string) (string, error) {
	m.replacementStopped = m.replaced
	return m.fakeContainmentManager.Stop(ctx, unit)
}
func TestCapturedUnitStopDoesNotTouchReplacementAfterObservation(t *testing.T) {
	t.Run("provider-supervisor", func(t *testing.T) {
		s, m, _ := systemdSupervisorFixture(t)
		wrapped := &replacementDuringStopManager{fakeContainmentManager: m}
		s.manager = wrapped
		if err := s.StartFake(context.Background(), authTestBinding()); err != nil {
			t.Fatal(err)
		}
		wrapped.armed = true
		if err := s.AbortAndWait(context.Background(), authTestBinding()); err == nil {
			t.Fatal("replacement accepted as original stop")
		}
		if !wrapped.replaced || wrapped.replacementStopped || m.stops != 0 {
			t.Fatal("name-targeted cleanup touched replacement", wrapped.replaced, wrapped.replacementStopped, m.stops)
		}
	})
	t.Run("git-acquisition", func(t *testing.T) {
		f := newGitUnitFixture(t)
		wrapped := &replacementDuringStopManager{fakeContainmentManager: f.manager}
		f.owner.manager = wrapped
		f.prepareStart(t)
		wrapped.armed = true
		if err := f.owner.stop(context.Background(), f.binding); err == nil {
			t.Fatal("replacement accepted as original stop")
		}
		if !wrapped.replaced || wrapped.replacementStopped || f.manager.stops != 0 {
			t.Fatal("name-targeted cleanup touched replacement", wrapped.replaced, wrapped.replacementStopped, f.manager.stops)
		}
	})
}

func TestCapturedUnitTerminationFailureRetainsOwnership(t *testing.T) {
	t.Run("provider-supervisor", func(t *testing.T) {
		s, m, _ := systemdSupervisorFixture(t)
		if err := s.StartFake(context.Background(), authTestBinding()); err != nil {
			t.Fatal(err)
		}
		m.kernel.terminate = func() error { return ErrAuthRecoveryRequired }
		if err := s.AbortAndWait(context.Background(), authTestBinding()); err == nil {
			t.Fatal("failed termination accepted")
		}
		record, err := s.snapshotUnit(context.Background(), authTestBinding())
		if err != nil || !record.Aborted || record.Stopped || m.stops != 0 {
			t.Fatal("uncertain ownership released", record, err)
		}
	})
	t.Run("git-acquisition", func(t *testing.T) {
		f := newGitUnitFixture(t)
		f.prepareStart(t)
		f.manager.kernel.terminate = func() error { return ErrAuthRecoveryRequired }
		if err := f.owner.stop(context.Background(), f.binding); err == nil {
			t.Fatal("failed termination accepted")
		}
		record, err := f.owner.read(f.binding.Attempt)
		if err != nil || !record.Revoked || record.Stopped || f.manager.stops != 0 {
			t.Fatal("uncertain ownership released", record, err)
		}
		if err := f.owner.Close(); err == nil {
			t.Fatal("uncertain unit owner discarded")
		}
	})
}
func TestEmptyOriginalActiveExitedUnitNeedsNoNameMutation(t *testing.T) {
	t.Run("provider-supervisor", func(t *testing.T) {
		s, m, _ := systemdSupervisorFixture(t)
		if err := s.StartFake(context.Background(), authTestBinding()); err != nil {
			t.Fatal(err)
		}
		m.terminalState = "active"
		if err := s.AbortAndWait(context.Background(), authTestBinding()); err != nil {
			t.Fatal("empty active-exited original rejected", err)
		}
		record, err := s.snapshotUnit(context.Background(), authTestBinding())
		if err != nil || !record.Stopped || record.StopJob != "" || m.observation.MainPID != 0 || m.stops != 1 {
			t.Fatal(record, err)
		}
		if err := s.ConfirmAuthStopped(context.Background(), authTestBinding()); err != nil || m.stops != 1 {
			t.Fatal("settled original stop replayed", err)
		}
	})
	t.Run("git-acquisition", func(t *testing.T) {
		f := newGitUnitFixture(t)
		f.prepareStart(t)
		f.manager.terminalState = "active"
		if err := f.owner.stop(context.Background(), f.binding); err != nil {
			t.Fatal("empty active-exited original rejected", err)
		}
		record, err := f.owner.read(f.binding.Attempt)
		if err != nil || !record.Stopped || record.StopJob != "" || f.manager.observation.MainPID != 0 || f.manager.stops != 1 {
			t.Fatal(record, err)
		}
		if err := f.owner.stop(context.Background(), f.binding); err != nil || f.manager.stops != 1 {
			t.Fatal("settled original stop replayed", err)
		}
	})
}

type settlingStopManager struct {
	*fakeContainmentManager
	pending int
}

func (m *settlingStopManager) Observe(ctx context.Context, unit string) (unitObservation, error) {
	current, err := m.fakeContainmentManager.Observe(ctx, unit)
	if err == nil && m.stops > 0 && m.pending > 0 {
		m.pending--
		current.MainPID = 1
		current.ActiveState = "deactivating"
	}
	return current, err
}
func TestOriginalUnitStopWaitsForManagerReap(t *testing.T) {
	t.Run("provider-supervisor", func(t *testing.T) {
		s, m, _ := systemdSupervisorFixture(t)
		wrapped := &settlingStopManager{m, 2}
		s.manager = wrapped
		if err := s.StartFake(context.Background(), authTestBinding()); err != nil {
			t.Fatal(err)
		}
		if err := s.AbortAndWait(context.Background(), authTestBinding()); err != nil || wrapped.pending != 0 || m.stops != 1 {
			t.Fatal("original manager reap was not awaited", err)
		}
	})
	t.Run("git-acquisition", func(t *testing.T) {
		f := newGitUnitFixture(t)
		wrapped := &settlingStopManager{f.manager, 2}
		f.owner.manager = wrapped
		f.prepareStart(t)
		if err := f.owner.stop(context.Background(), f.binding); err != nil || wrapped.pending != 0 || f.manager.stops != 1 {
			t.Fatal("original manager reap was not awaited", err)
		}
	})
}
