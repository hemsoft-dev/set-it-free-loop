//go:build linux

package reviewer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// ownEmptyCgroup creates an empty child of this test's own delegated cgroup.
// It has no processes, so the test can remove it at a chosen instant.
func ownEmptyCgroup(t *testing.T) (string, func() cgroupBinding) {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/cgroup")
	own := strings.TrimPrefix(strings.TrimSpace(string(raw)), "0::")
	if err != nil || !strings.HasPrefix(own, "/user.slice/") || strings.Contains(own, "\n") {
		t.Skip("no delegated cgroup v2 user hierarchy")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(own, "sfl-test-"+hex.EncodeToString(nonce[:]))
	full := filepath.Join("/sys/fs/cgroup", group)
	create := func() cgroupBinding {
		t.Helper()
		if err := os.Mkdir(full, 0700); err != nil {
			t.Skip("delegated cgroup is not writable", err)
		}
		root, hierarchy, err := openContainmentCgroupAt("/sys/fs/cgroup", group, cgroupHierarchy{})
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		inode, err := cgroupInode(root)
		if err != nil {
			t.Fatal(err)
		}
		return cgroupBinding{Path: group, Inode: inode, Hierarchy: hierarchy}
	}
	t.Cleanup(func() { os.Remove(full) })
	return full, create
}

// systemd removes an empty unit group about as soon as it is unpopulated, so
// its rmdir can land between opening the original and reading its files.
func TestOriginalCgroupRemovedAfterOpenIsStopped(t *testing.T) {
	for _, step := range []string{"empty", "kill", "terminate", "read"} {
		t.Run(step, func(t *testing.T) {
			full, create := ownEmptyCgroup(t)
			var events *os.File
			check := func(root *os.Root, binding cgroupBinding) error {
				switch step {
				case "empty":
					return openedCgroupEmpty(root, binding)
				case "kill":
					return killOpenedCgroup(context.Background(), root, binding)
				case "terminate":
					// Removed before Terminate's own first identity check. A
					// replacement keeps Terminate waiting, so bound it.
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					return linuxContainmentKernel{}.terminateOpened(ctx, root, binding)
				}
				// Removed after cgroup.events was opened, before it was read.
				if _, err := readCgroupFile(events); err != nil {
					return cgroupRemovedAfterOpen(err, binding)
				}
				return errors.New("removed original still readable")
			}
			binding := create()
			root, _, err := openContainmentCgroupAt("/sys/fs/cgroup", binding.Path, binding.Hierarchy)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if events, err = root.OpenFile("cgroup.events", os.O_RDONLY, 0); err != nil {
				t.Fatal(err)
			}
			defer events.Close()
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
			if err := check(root, binding); err != nil {
				t.Fatal("removed original rejected", err)
			}
			if err := (linuxContainmentKernel{}).Empty(binding); err != nil {
				t.Fatal("missing original rejected", err)
			}
			// Name reuse is a new instance. It never proves the original removed.
			replacement := create()
			if replacement.Inode == binding.Inode {
				t.Skip("kernel reused the cgroup inode")
			}
			if err := check(root, binding); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("replacement accepted as removed original", err)
			}
			if err := (linuxContainmentKernel{}).Empty(binding); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("replacement accepted as original", err)
			}
		})
	}
}
